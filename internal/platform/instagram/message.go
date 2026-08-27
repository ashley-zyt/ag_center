package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/message"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

// ─────────────────────────── 选择器配置 ───────────────────────────
// 依据真实页面 DOM(1/3/5/7/9/11.txt)校准。Instagram 采用 CSS-in-JS 的哈希类名,
// 稳定性有限, 但以下是当前真实页面可用的定位方式; 若页面改版需重新校准这里。

// igFollowButtonSelector 主页关注按钮: 未关注/已关注均为 <button> 且类名含 "_aswp"。
// 状态区分: 类名含 "_aswu" = 未关注(文案 Follow), 含 "_aswv" = 已关注(文案 Following)。
const igFollowButtonSelector = `button[class*="_aswp"]`

// igMessageButtonSelectors 对方主页"Message"按钮候选(仅已关注时出现)。
// 注意区分导航栏的 "Messages"(复数) 与按钮的 "Message"。
var igMessageButtonSelectors = []string{
	`//div[@role='button'][normalize-space(.)='Message']`,
	`//div[@role='button'][contains(., 'Message') and not(contains(., 'Messages'))]`,
}

// igMessageInputSelectors 消息输入框候选(Instagram 使用 Lexical contenteditable)。
var igMessageInputSelectors = []string{
	`div[data-lexical-editor="true"]`,
	`div[contenteditable="true"][role="textbox"][aria-placeholder*="Message"]`,
	`div[contenteditable="true"][role="textbox"]`,
}

// 消息方向标记(位于消息文本 div[dir="auto"] 的 class 后缀上):
//   - xyk4ms5: 我方发出(outgoing, 白字)
//   - x18lvrbx: 对方发来(incoming, 深色字)
// 说明: 气泡 role="presentation" 上的 x1lu5o8o/x1t39747 是"气泡圆角/分组位置"类, 并非方向,
// 不能用于判定方向(实测两者都会在 outgoing/incoming 中出现)。方向应看文本 div 的 class 后缀。
const igOutgoingClass = "xyk4ms5"
const igIncomingClass = "x18lvrbx"
const igMessageTextClass = "x1yc453h"

// ─────────────────────────── 入口函数 ───────────────────────────

// SendInstagramMessage 主动私信入口: 打开对方主页 -> 关注(如需) -> 点击Message -> 输入并发送
func SendInstagramMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &instagramMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// CheckInstagramReply 判断对方是否回复的入口:
// 打开对方主页 -> 点击 Message 进入会话 -> 解析消息判断回复状态与回复内容。
func CheckInstagramReply(ctx context.Context, logger *logx.Logger, opts message.CheckReplyOptions) (message.CheckReplyResult, error) {
	m := &instagramMessenger{logger: logger}
	return message.RunCheckReply(ctx, logger, m, opts), nil
}

// instagramMessenger 实现 message.MessengerActions
type instagramMessenger struct {
	logger *logx.Logger
}

func (m *instagramMessenger) Tag() string { return "IG_MSG" }

// CheckLogin 登录态检测: URL 被重定向到 /accounts/ 即未登录; 页面报错文案视为异常
func (m *instagramMessenger) CheckLogin(ctx context.Context) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var loc string
	if err := chromedp.Run(checkCtx, chromedp.Location(&loc)); err == nil {
		if strings.Contains(loc, "instagram.com/accounts/") {
			return message.StatusNotLoggedIn, nil
		}
	}

	var riskNodes []*cdp.Node
	_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[contains(text(), 'Something went wrong') or contains(text(), 'Sorry, something went wrong') or contains(text(), '出现了点问题')]`, &riskNodes, chromedp.BySearch))
	if len(riskNodes) > 0 {
		return message.StatusAbnormal, nil
	}
	return message.StatusLoggedIn, nil
}

// OpenTargetProfile 导航到对方主页
func (m *instagramMessenger) OpenTargetProfile(ctx context.Context, task message.SendTask) error {
	if err := chromedp.Run(ctx, chromedp.Navigate(task.TargetURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	m.logger.Print("IG_MSG2", "已打开目标用户主页: "+task.TargetURL)
	return nil
}

// OpenConversationFromProfile 在对方主页点击"Message"按钮进入会话。
// 仅发送流程(MessageContent 非空)需要"先关注再私信"; 判断回复流程不主动关注。
func (m *instagramMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	if strings.TrimSpace(task.MessageContent) != "" {
		if err := m.ensureFollowing(ctx); err != nil {
			return err
		}
	}
	return m.clickMessageButton(ctx)
}

// SendInConversation 输入内容并回车发送, 并校验消息已出现在会话记录中。
func (m *instagramMessenger) SendInConversation(ctx context.Context, content string) error {
	if err := m.waitAndFillMessage(ctx, content); err != nil {
		return err
	}

	m.logger.Print("IG_MSG5", "回车发送消息")
	enterCtx, cancelEnter := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEnter()
	if err := chromedp.Run(enterCtx, chromedp.KeyEvent("\r")); err != nil {
		return fmt.Errorf("回车发送失败: %v", err)
	}

	return m.verifySent(ctx, content)
}

// FetchConversationMessages 解析当前会话的消息(时间正序: 旧->新)。
// 方向判定基于消息文本 div[dir="auto"] 的 class 后缀: xyk4ms5=我方(outgoing), x18lvrbx=对方(incoming)。
func (m *instagramMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	js := fmt.Sprintf(`(function(outCls, inCls, textCls){
		var textDivs = Array.prototype.slice.call(document.querySelectorAll('div[dir="auto"][class*="' + textCls + '"]'));
		var out = [];
		for (var i = 0; i < textDivs.length; i++) {
			var el = textDivs[i];
			var cls = (el.className || '');
			var text = (el.innerText || el.textContent || '').trim();
			if (!text) continue;
			var isOut = cls.indexOf(outCls) >= 0;
			out.push({
				direction: isOut ? 'outgoing' : 'incoming',
				content: text,
				sent_at: ''
			});
		}
		return JSON.stringify(out);
	})(%q, %q, %q)`, igOutgoingClass, igIncomingClass, igMessageTextClass)

	var raw string
	evalCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := chromedp.Run(evalCtx, chromedp.Evaluate(js, &raw)); err != nil {
		return nil, err
	}
	var msgs []message.Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		return nil, fmt.Errorf("解析消息JSON失败: %v", err)
	}
	return msgs, nil
}

// ─────────────────────────── 辅助函数 ───────────────────────────

// ensureFollowing 尽力确保已关注对方(未关注则点击 Follow)。
// 注意: 实测部分账号点击 Follow 后按钮并不会变为 Following(关注请求挂起/未生效, 刷新后仍是 Follow),
// 但此时 Message 按钮仍可点击并正常发消息。因此这里采用"尽力而为"策略:
// 点击后短暂等待即返回, 不因"未变 Following"而阻断; 能否发消息交由 clickMessageButton 兜底判定。
func (m *instagramMessenger) ensureFollowing(ctx context.Context) error {
	m.logger.Print("IG_MSG2", "检查并关注对方账号")

	jsState := fmt.Sprintf(`(function(){
		var btns = document.querySelectorAll(%q);
		for (var i = 0; i < btns.length; i++) {
			var b = btns[i];
			var cls = (b.className || '');
			var text = (b.innerText || b.textContent || '').trim();
			if (cls.indexOf('_aswv') >= 0 || /^Following$/i.test(text)) return 'following';
			if (cls.indexOf('_aswu') >= 0 || /^Follow$/i.test(text)) return 'follow';
		}
		return 'missing';
	})()`, igFollowButtonSelector)

	// 先检测一次当前状态
	var st string
	detectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(detectCtx, chromedp.Evaluate(jsState, &st))
	cancel()

	switch st {
	case "following":
		m.logger.Print("IG_MSG2", "已关注对方账号")
		return nil
	case "missing":
		// 无关注按钮(可能为本人主页等), 不阻断, 交给后续 Message 判定
		m.logger.Print("IG_MSG2", "未找到关注按钮, 跳过关注直接尝试发消息")
		return nil
	case "follow":
		// 未关注, 点击 Follow
		m.logger.Print("IG_MSG2", "点击关注按钮")
		clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(clickCtx,
			chromedp.ScrollIntoView(igFollowButtonSelector, chromedp.ByQuery),
			chromedp.WaitVisible(igFollowButtonSelector, chromedp.ByQuery),
			chromedp.Click(igFollowButtonSelector, chromedp.ByQuery),
		)
		cancelClick()
		if err != nil {
			// 点击失败不阻断, 后续 clickMessageButton 会兜底判定能否发消息
			m.logger.Print("IG_MSG2", "点击关注按钮失败(不阻断): "+err.Error())
			return nil
		}
		m.logger.Print("IG_MSG2", "已点击关注按钮")
		time.Sleep(3 * time.Second)

		// 回读一次状态(仅日志): 未变 Following 也继续往下走
		var st2 string
		readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(readCtx, chromedp.Evaluate(jsState, &st2))
		cancelRead()
		if st2 == "following" {
			m.logger.Print("IG_MSG2", "已关注对方账号")
		} else {
			m.logger.Print("IG_MSG2", "关注后按钮未变为 Following(可能挂起/未生效), 继续尝试发消息")
		}
		return nil
	default:
		return nil
	}
}

// clickMessageButton 查找并点击对方主页的"Message"按钮
func (m *instagramMessenger) clickMessageButton(ctx context.Context) error {
	m.logger.Print("IG_MSG3", "查找Message按钮")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range igMessageButtonSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			cancel()

			if len(nodes) > 0 {
				m.logger.Print("IG_MSG3", "找到Message按钮: "+sel)
				clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
				err := chromedp.Run(clickCtx,
					chromedp.ScrollIntoView(sel, chromedp.BySearch),
					chromedp.WaitVisible(sel, chromedp.BySearch),
					chromedp.Click(sel, chromedp.BySearch),
				)
				cancelClick()
				if err == nil {
					m.logger.Print("IG_MSG3", "已点击Message按钮")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("IG_MSG3 60秒内未找到Message按钮(对方可能关闭了私信或未关注)")
}

// waitAndFillMessage 等待消息输入框并填写消息内容(Lexical contenteditable)。
// 参考 publish 的 fillReelTitle 注入方式: 点击聚焦 -> selectAll -> insertText -> 派发 input 事件 -> shake(空格+退格),
// 并在每轮注入后回读输入框内容做强校验, 确保文本真正写入 Lexical 编辑器, 避免"没输入进去却误判已发送"。
func (m *instagramMessenger) waitAndFillMessage(ctx context.Context, messageText string) error {
	m.logger.Print("IG_MSG4", "等待消息输入框")
	foundSelector := m.findMessageInput(ctx)
	if foundSelector == "" {
		return errors.New("IG_MSG4 60秒内未找到消息输入框")
	}
	m.logger.Print("IG_MSG4", "找到消息输入框: "+foundSelector)

	// 注入后回读文本的 JS
	readJS := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if (!el) return '';
		return (el.innerText || el.textContent || '').trim();
	})()`, foundSelector)

	// 内核级注入 JS(参考 fillReelTitle)
	injectJS := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if (!el) return false;
		el.focus();
		document.execCommand('selectAll', false, null);
		var ok = document.execCommand('insertText', false, %q);
		var ev = new InputEvent('input', { bubbles: true, cancelable: true });
		el.dispatchEvent(ev);
		return ok;
	})()`, foundSelector, messageText)

	// 先尝试 3 轮"内核注入 + shake + 回读校验"
	for retry := 0; retry < 3; retry++ {
		// 物理点击 + 强聚焦
		clickCtx, cancelClick := context.WithTimeout(ctx, 8*time.Second)
		_ = chromedp.Run(clickCtx,
			chromedp.Click(foundSelector, chromedp.ByQuery),
			chromedp.Focus(foundSelector, chromedp.ByQuery),
		)
		cancelClick()
		time.Sleep(300 * time.Millisecond)

		var injectOk bool
		injectCtx, cancelInject := context.WithTimeout(ctx, 8*time.Second)
		_ = chromedp.Run(injectCtx, chromedp.Evaluate(injectJS, &injectOk))
		cancelInject()

		// shake: 空格 + 退格, 触发编辑器状态刷新
		shakeCtx, cancelShake := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(shakeCtx,
			chromedp.SendKeys(foundSelector, " ", chromedp.ByQuery),
			chromedp.Sleep(100*time.Millisecond),
			chromedp.KeyEvent("\u0008"),
		)
		cancelShake()

		// 回读校验
		var currentText string
		checkCtx, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(readJS, &currentText))
		cancelCheck()

		if currentText != "" {
			m.logger.Print("IG_MSG4", "已填写消息内容: "+currentText)
			return nil
		}
		m.logger.Print("IG_MSG4", fmt.Sprintf("注入后回读为空, 第 %d 次重试...", retry+1))
		time.Sleep(500 * time.Millisecond)
	}

	// 兜底: 键盘逐字输入(真实按键事件, Lexical 能可靠捕获)
	m.logger.Print("IG_MSG4", "改用键盘逐字输入")
	sendCtx, cancelSend := context.WithTimeout(ctx, 15*time.Second)
	err := chromedp.Run(sendCtx,
		chromedp.Click(foundSelector, chromedp.ByQuery),
		chromedp.SendKeys(foundSelector, messageText, chromedp.ByQuery),
	)
	cancelSend()
	if err != nil {
		return fmt.Errorf("IG_MSG4 键盘输入消息失败: %v", err)
	}

	// 再次回读校验, 若仍为空则直接报错, 不进入发送步骤(避免空发送/误判已发送)
	var finalText string
	finalCtx, cancelFinal := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(finalCtx, chromedp.Evaluate(readJS, &finalText))
	cancelFinal()
	if finalText == "" {
		return errors.New("IG_MSG4 消息内容未能写入输入框")
	}
	m.logger.Print("IG_MSG4", "已使用键盘输入消息: "+finalText)
	return nil
}

// findMessageInput 轮询查找消息输入框并返回命中的选择器(优先 CSS, 全部按 ByQuery 处理)
func (m *instagramMessenger) findMessageInput(ctx context.Context) string {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range igMessageInputSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery))
			cancel()
			if len(nodes) > 0 {
				return sel
			}
		}
		time.Sleep(1 * time.Second)
	}
	return ""
}

// verifySent 校验发送成功: 轮询读取会话最后一条消息, 要求其为"我方发出"且文本前缀与本次发送内容一致。
func (m *instagramMessenger) verifySent(ctx context.Context, content string) error {
	needle := strings.TrimSpace(content)
	if needle == "" {
		return nil
	}
	wantPrefix := prefixRunes(needle, 5)

	js := fmt.Sprintf(`(function(outCls, inCls, textCls){
		var textDivs = Array.prototype.slice.call(document.querySelectorAll('div[dir="auto"][class*="' + textCls + '"]'));
		for (var i = textDivs.length - 1; i >= 0; i--) {
			var el = textDivs[i];
			var cls = (el.className || '');
			var text = (el.innerText || el.textContent || '').trim();
			if (!text) continue;
			var isOut = cls.indexOf(outCls) >= 0;
			return JSON.stringify({ok:true, outgoing:isOut, text:text});
		}
		return JSON.stringify({ok:false, reason:'no_message'});
	})(%q, %q, %q)`, igOutgoingClass, igIncomingClass, igMessageTextClass)

	deadline := time.Now().Add(20 * time.Second)
	lastReason := ""
	for time.Now().Before(deadline) {
		var raw string
		evalCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(evalCtx, chromedp.Evaluate(js, &raw))
		cancel()

		var v struct {
			OK       bool   `json:"ok"`
			Reason   string `json:"reason"`
			Outgoing bool   `json:"outgoing"`
			Text     string `json:"text"`
		}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &v)
		}

		if v.OK {
			lastReason = ""
			if v.Outgoing {
				if prefixRunes(v.Text, 5) == wantPrefix {
					m.logger.Print("IG_MSG5", "消息已发送成功(最后一条为我方且前5字符匹配)")
					return nil
				}
				lastReason = fmt.Sprintf("最后一条为我方消息但内容不匹配(期望前5=%q, 实际前5=%q)", wantPrefix, prefixRunes(v.Text, 5))
			} else {
				lastReason = "会话最后一条仍是对方消息"
			}
		} else {
			lastReason = v.Reason
		}

		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return fmt.Errorf("验证发送结果时上下文超时: %v", ctx.Err())
		}
	}
	if lastReason != "" {
		return fmt.Errorf("20秒内未确认消息发送成功: %s", lastReason)
	}
	return errors.New("20秒内未确认消息发送成功")
}

// prefixRunes 返回字符串前 n 个字符(按 rune 计数, 兼容多字节)。
func prefixRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" || n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n])
}
