package tiktok

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
// 会话列表拉取已移除, 此处仅保留发送/回复流程用到的选择器(在下方各函数内联定义)。


// ─────────────────────────── 入口函数 ───────────────────────────

// SendTikTokMessage 批量主动私信入口: 打开对方主页 -> 点击发消息 -> 输入发送
func SendTikTokMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &tiktokMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// CheckTikTokReply 判断对方是否回复的入口:
// 打开对方主页 -> 点击 Message 进入会话 -> 解析最新消息判断回复状态与回复内容。
// 复用 message.RunCheckReply 统一流程, 具体页面元素与操作步骤待确认后校准选择器。
func CheckTikTokReply(ctx context.Context, logger *logx.Logger, opts message.CheckReplyOptions) (message.CheckReplyResult, error) {
	m := &tiktokMessenger{logger: logger}
	return message.RunCheckReply(ctx, logger, m, opts), nil
}

// tiktokMessenger 实现 message.MessengerActions
type tiktokMessenger struct {
	logger *logx.Logger
	// partnerHandle 当前会话对方账号(不带@，小写)。用于判断消息方向/发送验证。
	partnerHandle string
}

func (t *tiktokMessenger) Tag() string      { return "TT_MSG" }

func normalizeTikTokHandle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	s = strings.ToLower(s)
	return s
}

func extractTikTokHandleFromURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// 常见主页: https://www.tiktok.com/@username 或 https://www.tiktok.com/@username?lang=en
	if i := strings.Index(u, "/@"); i >= 0 {
		rest := u[i+2:]
		if j := strings.IndexAny(rest, "/?&"); j >= 0 {
			rest = rest[:j]
		}
		return normalizeTikTokHandle(rest)
	}
	return ""
}

func (t *tiktokMessenger) setPartnerHandle(task message.SendTask) {
	if task.AccountName != "" {
		t.partnerHandle = normalizeTikTokHandle(task.AccountName)
		return
	}
	t.partnerHandle = extractTikTokHandleFromURL(task.TargetURL)
}

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

// CheckLogin 检测当前页面登录态: DOM探针找登录按钮 / 风控文案。
// 探针使用完整文案'Log in to TikTok'并排除 script/style:
// 页面内嵌的 JSON 翻译串(如 common_login_panel_title:"Log in to TikTok")位于 <script type="application/json"> 中,
// 若不排除会把正常登录态误判为未登录。
func (t *tiktokMessenger) CheckLogin(ctx context.Context) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var loginNodes []*cdp.Node
	_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[not(self::script) and not(self::style)][contains(text(), 'Log in to TikTok')]`, &loginNodes, chromedp.BySearch))
	if len(loginNodes) > 0 {
		return message.StatusNotLoggedIn, nil
	}

	var riskNodes []*cdp.Node
	_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[not(self::script) and not(self::style)][contains(text(), 'Something went wrong') or contains(text(), '出现了一点问题') or contains(text(), 'We regret to inform you that we have discontinued operating TikTok in Hong Kong.')]`, &riskNodes, chromedp.BySearch))
	if len(riskNodes) > 0 {
		return message.StatusAbnormal, nil
	}
	return message.StatusLoggedIn, nil
}

// OpenTargetProfile 导航到对方主页
func (t *tiktokMessenger) OpenTargetProfile(ctx context.Context, task message.SendTask) error {
	t.setPartnerHandle(task)
	if err := chromedp.Run(ctx, chromedp.Navigate(task.TargetURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	t.logger.Print("TT_MSG2", "已打开目标用户主页: "+task.TargetURL)
	return nil
}

// OpenConversationFromProfile 在对方主页点击"Message/发消息"按钮进入会话
func (t *tiktokMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	// 仅发送流程需要“先关注再私信”；判断回复/拉取消息不应主动触发关注。
	if strings.TrimSpace(task.MessageContent) != "" {
		if err := ensureFollowing(ctx, t.logger); err != nil {
			return err
		}
	}
	return clickMessageButton(ctx, t.logger)
}

// SendInConversation 在已打开的会话中输入内容并回车发送, 并确认消息出现在聊天记录中。
// TikTok 私信输入框为 Draft.js(contenteditable), Enter 即发送。
func (t *tiktokMessenger) SendInConversation(ctx context.Context, content string) error {
	if err := waitAndFillMessage(ctx, t.logger, content); err != nil {
		return err
	}

	t.logger.Print("TT_MSG4", "回车发送消息")
	enterCtx, cancelEnter := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEnter()
	if err := chromedp.Run(enterCtx, chromedp.KeyEvent("\r")); err != nil {
		return fmt.Errorf("回车发送失败: %v", err)
	}

	return verifySent(ctx, t.logger, t.partnerHandle, content)
}

// FetchConversationMessages 解析当前会话的最新消息(时间正序: 旧->新)
func (t *tiktokMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	partner := t.partnerHandle
	containerSel := `div[data-e2e="dm-new-message-list"]`
	js := fmt.Sprintf(`(function(contSel, partner){
		var cont = document.querySelector(contSel);
		if (!cont) return "[]";
		var items = Array.prototype.slice.call(cont.querySelectorAll('[data-e2e="dm-new-chat-item"]'));
		if (!items.length) return "[]";

		function normHandle(h){
			h = (h || '').trim();
			h = h.replace(/^@+/, '');
			h = h.toLowerCase();
			return h;
		}
		partner = normHandle(partner);

		var out = [];
		for (var i = 0; i < items.length; i++) {
			var it = items[i];
			// 跳过提示类项（比如 "Message request accepted..."），它没有 dm-new-message-text
			var p = it.querySelector('[data-e2e="dm-new-message-text"] p');
			if (!p) continue;
			var content = (p.innerText || p.textContent || '').trim();
			if (!content) continue;

			// 通过消息内头像链接判断发送方: a[href^="/@xxx"]
			var a = it.querySelector('a[href^="/@"]');
			var sender = '';
			if (a && a.getAttribute('href')) {
				sender = a.getAttribute('href');
				if (sender.indexOf('/@') >= 0) sender = sender.split('/@')[1];
				sender = sender.split('?')[0].split('/')[0];
			}
			sender = normHandle(sender);

			var outgoing = false;
			if (partner) {
				// 单人私信场景: sender==partner 视为对方(incoming)，否则为我方(outgoing)
				outgoing = sender !== partner;
			} else {
				// 兜底: 使用 class 关键字猜测方向
				var cls = '';
				try { cls = (it.className || '') + ' ' + (it.parentElement ? it.parentElement.className : ''); } catch(e) {}
				outgoing = /self|right|mine|outgoing/i.test(cls);
			}

			out.push({
				direction: outgoing ? 'outgoing' : 'incoming',
				content: content,
				sent_at: ''
			});
		}
		return JSON.stringify(out);
	})(%q, %q)`, containerSel, partner)

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

// ─────────────────────────── 主页发消息辅助 ───────────────────────────

// ensureFollowing 尽力确保已关注对方(未关注则点击 Follow)。
// 关注按钮:
// - 未关注: button[data-e2e="follow-button"] aria-label="Follow xxx", 文案 Follow
// - 已关注: button[data-e2e="follow-button"] aria-label="Following xxx", 文案 Following
// 注意: 部分账号点击 Follow 后按钮不会变为 Following(关注请求挂起/未生效), 但页面上的 Message 按钮仍可点击进入会话,
// 因此这里采用"尽力而为"策略: 点击后短暂等待即返回, 不因"未变 Following"而阻断; 能否发消息由 clickMessageButton 兜底判定。
func ensureFollowing(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT_MSG2", "检查并关注对方账号")
	btnSel := `button[data-e2e="follow-button"]`
	jsState := fmt.Sprintf(`(function(){
		var btn = document.querySelector(%q);
		if (!btn) return "missing";
		var label = (btn.getAttribute('aria-label') || '').trim();
		var text = (btn.innerText || btn.textContent || '').trim();
		if (/^Following\\b/i.test(label) || /^Following\\b/i.test(text)) return "following";
		if (/^Follow\\b/i.test(label) || /^Follow\\b/i.test(text)) return "follow";
		return "unknown:" + label + ":" + text;
	})()`, btnSel)

	// 先检测一次当前状态
	var st string
	detectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(detectCtx, chromedp.Evaluate(jsState, &st))
	cancel()

	switch {
	case st == "following":
		logger.Print("TT_MSG2", "已关注对方账号")
		return nil
	case st == "missing":
		// 无关注按钮(可能为本人主页等), 不阻断, 交给后续 Message 判定
		logger.Print("TT_MSG2", "未找到关注按钮, 跳过关注直接尝试发消息")
		return nil
	case st == "follow":
		// 未关注, 点击 Follow(尽力而为)
		logger.Print("TT_MSG2", "点击关注按钮")
		clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(clickCtx,
			chromedp.ScrollIntoView(btnSel, chromedp.ByQuery),
			chromedp.WaitVisible(btnSel, chromedp.ByQuery),
			chromedp.Click(btnSel, chromedp.ByQuery),
		)
		cancelClick()
		if err != nil {
			// 点击失败不阻断, 后续 clickMessageButton 会兜底判定能否发消息
			logger.Print("TT_MSG2", "点击关注按钮失败(不阻断): "+err.Error())
			return nil
		}
		logger.Print("TT_MSG2", "已点击关注按钮")
		time.Sleep(3 * time.Second)

		// 回读一次状态(仅日志): 未变 Following 也继续往下走
		var st2 string
		readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(readCtx, chromedp.Evaluate(jsState, &st2))
		cancelRead()
		if st2 == "following" {
			logger.Print("TT_MSG2", "已关注对方账号")
		} else {
			logger.Print("TT_MSG2", "关注后按钮未变为 Following(可能挂起/未生效), 继续尝试发消息")
		}
		return nil
	default:
		// 未知状态(如 unknown:xxx), 不阻断, 交给后续 Message 判定
		logger.Print("TT_MSG2", "关注按钮状态未知("+st+"), 继续尝试发消息")
		return nil
	}
}

// clickMessageButton 查找并点击"Message"或"发消息"按钮
func clickMessageButton(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT_MSG3", "查找Message按钮")
	candidates := []string{
		// 优先结构化定位(data-e2e), 不依赖英文/中文文案
		`a[href^="/messages"] button[data-e2e="message-button"]`,
		`a[href*="/messages"] button[data-e2e="message-button"]`,
		`button[data-e2e="message-button"]`,
		`div[data-e2e="message-button"]`,
		`a[href^="/messages"]`,
		`a[href*="/messages"]`,
		`//button[@data-e2e='message-button']`,
		`//div[@data-e2e='message-button']`,
		`//a[contains(@href, '/messages')]//button[@data-e2e='message-button']`,
		`//a[contains(@href, '/messages')]`,
		// 文案兜底(兼容多语言: Message / 发消息 / 私信 / Direct message)
		`//button[contains(., 'Message') or contains(., '发消息') or contains(., '私信') or contains(., 'Direct message')]`,
		`//div[@role='button' and (contains(., 'Message') or contains(., '发消息') or contains(., '私信'))]`,
		`//a[contains(@href, '/direct') or contains(@href, '/message') or contains(@href, '/messages')]`,
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range candidates {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if strings.HasPrefix(sel, "//") {
				_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			} else {
				_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery))
			}
			cancel()

			if len(nodes) > 0 {
				logger.Print("TT_MSG3", "找到Message按钮: "+sel)
				clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
				var err error
				if strings.HasPrefix(sel, "//") {
					err = chromedp.Run(clickCtx,
						chromedp.ScrollIntoView(sel, chromedp.BySearch),
						chromedp.WaitVisible(sel, chromedp.BySearch),
						chromedp.Click(sel, chromedp.BySearch),
					)
				} else {
					err = chromedp.Run(clickCtx,
						chromedp.ScrollIntoView(sel, chromedp.ByQuery),
						chromedp.WaitVisible(sel, chromedp.ByQuery),
						chromedp.Click(sel, chromedp.ByQuery),
					)
				}
				cancelClick()
				if err == nil {
					logger.Print("TT_MSG3", "已点击Message按钮")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("TT_MSG3 60秒内未找到Message按钮(对方可能关闭了私信或未登录)")
}

// waitAndFillMessage 等待消息输入框并填写消息内容
func waitAndFillMessage(ctx context.Context, logger *logx.Logger, messageText string) error {
	logger.Print("TT_MSG4", "等待消息输入框")
	inputSelectors := []string{
		// 结构化定位(不依赖占位文案, 兼容多语言): Draft.js contenteditable 输入框
		`div[data-e2e="dm-new-input-editor"] div[contenteditable="true"]`,
		`div[data-e2e="message-input-area"] div[contenteditable="true"]`,
		`div[aria-label="Send a message..."][contenteditable="true"]`,
		`//div[@role='textbox' and @contenteditable='true']`,
		`//div[data-e2e='dm-new-input-editor']`,
		`//div[data-e2e='message-input']`,
		`//textarea[@placeholder='Message' or @placeholder='发消息' or @placeholder='私信']`,
		`//input[@type='text' and (@placeholder='Message' or @placeholder='发消息')]`,
	}

	var foundSelector string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range inputSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if strings.HasPrefix(sel, "//") {
				_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			} else {
				_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery))
			}
			cancel()

			if len(nodes) > 0 {
				foundSelector = sel
				break
			}
		}
		if foundSelector != "" {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if foundSelector == "" {
		return errors.New("TT_MSG4 60秒内未找到消息输入框")
	}

	logger.Print("TT_MSG4", "找到消息输入框: "+foundSelector)

	// 物理点击聚焦输入框(失败不阻断, JS 内仍有 el.focus() 兜底)
	clickCtx, cancelClick := context.WithTimeout(ctx, 8*time.Second)
	if strings.HasPrefix(foundSelector, "//") {
		_ = chromedp.Run(clickCtx, chromedp.Click(foundSelector, chromedp.BySearch))
	} else {
		_ = chromedp.Run(clickCtx, chromedp.Click(foundSelector, chromedp.ByQuery))
	}
	cancelClick()

	logger.Print("TT_MSG4", "填写消息内容")

	// 使用JavaScript插入文本，兼容contenteditable/Draft.js
	var ok bool
	js := fmt.Sprintf(`(function(sel, msg){
		var el = null;
		try { el = document.evaluate(sel, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue; } catch(e) { el = null; }
		if(!el) { try { el = document.querySelector(sel); } catch(e2) { el = null; } }
		if(!el) return false;
		el.focus();
		try{
			var s = window.getSelection();
			if(s) {
				s.removeAllRanges();
				var r = document.createRange();
				r.selectNodeContents(el);
				s.addRange(r);
			}
		}catch(e){}
		try{
			if(document.execCommand('insertText', false, msg)) return true;
		}catch(e){}
		el.textContent = msg;
		try{
			el.dispatchEvent(new InputEvent('input', {bubbles:true}));
		}catch(e){
			el.dispatchEvent(new Event('input', {bubbles:true}));
		}
		return true;
	})(%q, %q)`, foundSelector, messageText)

	evalCtx, cancelEval := context.WithTimeout(ctx, 10*time.Second)
	if err := chromedp.Run(evalCtx, chromedp.Evaluate(js, &ok)); err != nil {
		cancelEval()
		return fmt.Errorf("TT_MSG4 注入消息文本失败: %v", err)
	}
	cancelEval()

	if !ok {
		// 兜底使用SendKeys
		sendCtx, cancelSend := context.WithTimeout(ctx, 10*time.Second)
		if strings.HasPrefix(foundSelector, "//") {
			err := chromedp.Run(sendCtx, chromedp.SendKeys(foundSelector, messageText, chromedp.BySearch))
			cancelSend()
			if err != nil {
				return fmt.Errorf("TT_MSG4 键盘输入消息失败: %v", err)
			}
		} else {
			err := chromedp.Run(sendCtx, chromedp.SendKeys(foundSelector, messageText, chromedp.ByQuery))
			cancelSend()
			if err != nil {
				return fmt.Errorf("TT_MSG4 键盘输入消息失败: %v", err)
			}
		}
		logger.Print("TT_MSG4", "已使用键盘输入消息")
	} else {
		logger.Print("TT_MSG4", "已填写消息内容")
	}

	return nil
}

// clickSendButton 查找并点击发送按钮
func clickSendButton(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT_MSG5", "查找发送按钮")
	candidates := []string{
		`//button[contains(., 'Send') or contains(., '发送') or contains(., '发送消息')]`,
		`//div[@role='button' and (contains(., 'Send') or contains(., '发送'))]`,
		`//button[@data-e2e='send-button']`,
		`//div[@data-e2e='send-button']`,
		`//button[@type='submit']`,
		`//button//span[contains(., 'Send') or contains(., '发送')]`,
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range candidates {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if strings.HasPrefix(sel, "//") {
				_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			} else {
				_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery))
			}
			cancel()

			if len(nodes) > 0 {
				logger.Print("TT_MSG5", "找到发送按钮: "+sel)
				clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
				err := chromedp.Run(clickCtx,
					chromedp.ScrollIntoView(sel, chromedp.BySearch),
					chromedp.WaitVisible(sel, chromedp.BySearch),
					chromedp.Click(sel, chromedp.BySearch),
				)
				cancelClick()
				if err == nil {
					logger.Print("TT_MSG5", "已点击发送按钮")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("TT_MSG5 60秒内未找到发送按钮")
}

// verifySent 校验发送成功:
// 轮询读取会话最后一条消息，要求：
// 1) 最后一条为我方(outgoing)：通过“消息内头像链接 a[href^='/@'] 与 partnerHandle”比对判定；
// 2) 最后一条文本与本次发送内容的前 N(默认5)个字符一致。
//
// 注意：若发送后对方立刻回复，最后一条可能变为对方消息，会按“未确认发送成功”处理（符合你给的 3/4/5.txt 用例）。
func verifySent(ctx context.Context, logger *logx.Logger, partnerHandle string, content string) error {
	needle := strings.TrimSpace(content)
	if needle == "" {
		return nil
	}
	wantPrefix := prefixRunes(needle, 5)
	partnerHandle = normalizeTikTokHandle(partnerHandle)
	containerSel := `div[data-e2e="dm-new-message-list"]`

	js := fmt.Sprintf(`(function(contSel, partner){
		function normHandle(h){
			h = (h || '').trim();
			h = h.replace(/^@+/, '');
			h = h.toLowerCase();
			return h;
		}
		partner = normHandle(partner);

		var cont = document.querySelector(contSel);
		if(!cont) return JSON.stringify({ok:false, reason:"no_container"});

		var items = Array.prototype.slice.call(cont.querySelectorAll('[data-e2e="dm-new-chat-item"]'));
		// 从后往前找最后一条真正的消息(带 dm-new-message-text)
		for (var i = items.length - 1; i >= 0; i--) {
			var it = items[i];
			var p = it.querySelector('[data-e2e="dm-new-message-text"] p');
			if (!p) continue;
			var text = (p.innerText || p.textContent || '').trim();
			if (!text) continue;

			var a = it.querySelector('a[href^=\"/@\"]');
			var sender = '';
			if (a && a.getAttribute('href')) {
				sender = a.getAttribute('href');
				if (sender.indexOf('/@') >= 0) sender = sender.split('/@')[1];
				sender = sender.split('?')[0].split('/')[0];
			}
			sender = normHandle(sender);
			var outgoing = partner ? (sender !== partner) : false;

			return JSON.stringify({ok:true, outgoing:outgoing, text:text, sender:sender});
		}
		return JSON.stringify({ok:false, reason:\"no_message\"});
	})(%q, %q)`, containerSel, partnerHandle)

	deadline := time.Now().Add(20 * time.Second)
	lastReason := ""
	lastTextPrefix := ""
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
			Sender   string `json:"sender"`
		}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &v)
		}

		if v.OK {
			lastReason = ""
			lastTextPrefix = prefixRunes(v.Text, 5)
			if v.Outgoing {
				if lastTextPrefix == wantPrefix {
					logger.Print("TT_MSG5", "消息已发送成功(最后一条为我方且前5字符匹配)")
					return nil
				}
				lastReason = fmt.Sprintf("最后一条为我方消息但内容不匹配(期望前5=%q, 实际前5=%q)", wantPrefix, lastTextPrefix)
			} else {
				lastReason = fmt.Sprintf("会话最后一条仍是对方消息(sender=%q)", v.Sender)
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
