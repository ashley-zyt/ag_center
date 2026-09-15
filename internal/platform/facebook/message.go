package facebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"minimax_pro/internal/chromedputil"
	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/message"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// ─────────────────────────── 选择器配置 ───────────────────────────
// TODO(校准): 以下选择器为初始候选, 需在真实页面实测后校准。校准时只改这些数组。
// FB 会话链接 /messages/t/<id> 稳定可导航, 会话定位优先走链接。

// fbAddFriendButtonSelector 对方主页"添加好友"按钮(aria-label 形如 "Add Friend <用户名>")。
// 前缀匹配 "Add Friend"，兼容不同语言/用户名；配合 ByQuery 使用。
const fbAddFriendButtonSelector = `div[role="button"][aria-label^="Add Friend"]`

// fbMessageButtonSelectors 对方主页"发消息"按钮候选
var fbMessageButtonSelectors = []string{
	`//div[@role='button'][@aria-label='Message' or @aria-label='发消息']`,
	`//a[contains(@href, '/messages/t/')]`,
	`//span[text()='Message' or text()='发消息']/ancestor::div[@role='button'][1]`,
}

// fbMessageInputSelectors 消息输入框候选(CSS, 配合 ByQuery)。
// 对话框输入框 aria-label 形如 "Write to <用户名>", 需优先匹配, 避免误选评论框("Write a comment…")。
var fbMessageInputSelectors = []string{
	`div[role="textbox"][aria-label^="Write to"]`,
	`div[data-lexical-editor="true"][aria-placeholder="Aa"]`,
	`div[role="textbox"][contenteditable="true"]`,
}

// fbMessageItemSelector 会话内单条消息容器(3.txt 实测确认)。
// 每条消息是 div[aria-roledescription="message"], 其 aria-label 形如
// "At 3:08 AM, Han: hello~ first message"(发送者=对方) 或 "... You: ..."(发送者=自己)。
const fbMessageItemSelector = `div[aria-roledescription="message"]`

// ─────────────────────────── 入口函数 ───────────────────────────

// SendFacebookMessage 批量主动私信入口
func SendFacebookMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &facebookMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// CheckFacebookReply 判断对方是否回复的入口
func CheckFacebookReply(ctx context.Context, logger *logx.Logger, opts message.CheckReplyOptions) (message.CheckReplyResult, error) {
	m := &facebookMessenger{logger: logger}
	return message.RunCheckReply(ctx, logger, m, opts), nil
}

// facebookMessenger 实现 message.MessengerActions
type facebookMessenger struct {
	logger *logx.Logger
}

func (m *facebookMessenger) Tag() string      { return "FB_MSG" }

// CheckLogin 登录态检测: 未登录会被重定向到 /login 或渲染登录表单; checkpoint 视为风控异常
func (m *facebookMessenger) CheckLogin(ctx context.Context) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var loc string
	if err := chromedp.Run(checkCtx, chromedp.Location(&loc)); err == nil {
		if strings.Contains(loc, "checkpoint") {
			return message.StatusAbnormal, nil
		}
		if strings.Contains(loc, "/login") {
			return message.StatusNotLoggedIn, nil
		}
	}

	var status string
	js := `(function(){
		var loginForm = document.getElementById('login_form') || document.querySelector('input[name="email"]');
		if (loginForm) return 'not_logged_in';
		return 'logged_in';
	})()`
	if err := chromedp.Run(checkCtx, chromedp.Evaluate(js, &status)); err != nil {
		return message.StatusLoggedIn, nil
	}
	if status == "not_logged_in" {
		return message.StatusNotLoggedIn, nil
	}
	return message.StatusLoggedIn, nil
}

// OpenTargetProfile 导航到对方主页
func (m *facebookMessenger) OpenTargetProfile(ctx context.Context, task message.SendTask) error {
	if err := chromedputil.NavigateAndWaitBody(ctx, m.logger, task.TargetURL, "FB_MSG2"); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	m.logger.Print("FB_MSG2", "已打开目标用户主页: "+task.TargetURL)
	return nil
}

// OpenConversationFromProfile 在对方主页先确保已发送好友请求(如尚未加好友则点 Add friend),
// 再点击"发消息"按钮进入会话。
func (m *facebookMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	if strings.TrimSpace(task.MessageContent) != "" {
		if err := m.ensureFriendRequest(ctx); err != nil {
			return err
		}
	}
	return m.clickMessageButton(ctx)
}

// SendInConversation 输入内容并回车发送, 并校验消息已发出(输入框被清空)。
func (m *facebookMessenger) SendInConversation(ctx context.Context, content string) error {
	if err := m.waitAndFillMessage(ctx, content); err != nil {
		return err
	}

	m.logger.Print("FB_MSG5", "回车发送消息")
	enterCtx, cancelEnter := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEnter()
	if err := chromedp.Run(enterCtx, chromedp.KeyEvent("\r")); err != nil {
		return fmt.Errorf("回车发送失败: %v", err)
	}

	return m.verifySent(ctx, content)
}

// FetchConversationMessages 解析当前会话的消息(时间正序: 旧->新)。
// 每条消息容器 div[aria-roledescription="message"] 的 aria-label 形如 "At <时间>, <发送者>: <内容>"，
// 发送者为 "You"/"你" 表示自己发出(outgoing)，否则为对方发来(incoming)。
func (m *facebookMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	js := fmt.Sprintf(`(function(){
		var nodes = document.querySelectorAll(%q);
		var out = [];
		for (var i = 0; i < nodes.length; i++) {
			var label = nodes[i].getAttribute('aria-label') || '';
			if (!label) continue;
			// 提取 ", <发送者>: <内容>"，冒号兼容中英文(: 或 ：)
			var m = label.match(/,\s*([^:：]+)[:：]\s*([\s\S]*)$/);
			if (!m) continue;
			var sender = m[1].trim();
			var content = m[2].trim();
			var outgoing = (sender === 'You' || sender === '你');
			var timeMatch = label.match(/^At\s+([^,]+)/);
			out.push({
				direction: outgoing ? 'outgoing' : 'incoming',
				content: content,
				sent_at: timeMatch ? timeMatch[1].trim() : ''
			});
		}
		return JSON.stringify(out);
	})()`, fbMessageItemSelector)

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

// clickMessageButton 查找并点击对方主页的"发消息"按钮
func (m *facebookMessenger) clickMessageButton(ctx context.Context) error {
	m.logger.Print("FB_MSG3", "查找Message按钮")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range fbMessageButtonSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			cancel()

			if len(nodes) > 0 {
				m.logger.Print("FB_MSG3", "找到Message按钮: "+sel)
				clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
				err := chromedp.Run(clickCtx,
					chromedp.ScrollIntoView(sel, chromedp.BySearch),
					chromedp.WaitVisible(sel, chromedp.BySearch),
					chromedp.Click(sel, chromedp.BySearch),
				)
				cancelClick()
				if err == nil {
					m.logger.Print("FB_MSG3", "已点击Message按钮")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("FB_MSG3 60秒内未找到Message按钮(对方可能关闭了私信)")
}

// ensureFriendRequest 若对方主页存在"添加好友"按钮则点击并等待 10 秒, 否则跳过。
// 三种状态: 未加好友(有 Add Friend) / 已发请求(变 Cancel Request) / 已是好友(无 Add Friend)。
func (m *facebookMessenger) ensureFriendRequest(ctx context.Context) error {
	m.logger.Print("FB_MSG2", "检查是否需添加好友")

	var found bool
	detectJS := fmt.Sprintf(`(function(sel){
		var b = document.querySelector(sel);
		return !!b;
	})(%q)`, fbAddFriendButtonSelector)

	detectCtx, cancelDetect := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(detectCtx, chromedp.Evaluate(detectJS, &found))
	cancelDetect()

	if !found {
		m.logger.Print("FB_MSG2", "未找到 Add friend 按钮(已是好友或请求已发), 跳过")
		return nil
	}

	m.logger.Print("FB_MSG2", "点击 Add friend 按钮")
	clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
	err := chromedp.Run(clickCtx,
		chromedp.ScrollIntoView(fbAddFriendButtonSelector, chromedp.ByQuery),
		chromedp.WaitVisible(fbAddFriendButtonSelector, chromedp.ByQuery),
		chromedp.Click(fbAddFriendButtonSelector, chromedp.ByQuery),
	)
	cancelClick()
	if err != nil {
		// 点击失败不阻断, 后续 clickMessageButton 会兜底判定能否发消息
		m.logger.Print("FB_MSG2", "点击 Add friend 失败(不阻断): "+err.Error())
		return nil
	}
	m.logger.Print("FB_MSG2", "已点击 Add friend, 等待 10 秒")
	time.Sleep(10 * time.Second)
	return nil
}

// findMessageInput 轮询查找消息输入框并返回命中的选择器(全部按 CSS ByQuery 处理)
func (m *facebookMessenger) findMessageInput(ctx context.Context) string {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range fbMessageInputSelectors {
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

// waitAndFillMessage 等待消息输入框, 用真实键盘事件逐字填入内容并回读校验。
// 关键: 消息可能含多行(\n), 而 chromedp 的 SendKeys 会把 \n 强制转成 Enter(发送),
// 因此必须拆行逐行输入, 行间用 Shift+Enter 显式换行(FB Messenger: Enter 发送, Shift+Enter 换行)。
func (m *facebookMessenger) waitAndFillMessage(ctx context.Context, messageText string) error {
	m.logger.Print("FB_MSG4", "等待消息输入框")
	foundSelector := m.findMessageInput(ctx)
	if foundSelector == "" {
		return errors.New("FB_MSG4 60秒内未找到消息输入框")
	}
	m.logger.Print("FB_MSG4", "找到消息输入框: "+foundSelector)

	readJS := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if (!el) return '';
		return (el.innerText || el.textContent || '').trim();
	})()`, foundSelector)

	// 聚焦输入框(真实点击+聚焦, 确保键盘事件落在输入框上)
	focusCtx, cancelFocus := context.WithTimeout(ctx, 8*time.Second)
	_ = chromedp.Run(focusCtx,
		chromedp.Click(foundSelector, chromedp.ByQuery),
		chromedp.Focus(foundSelector, chromedp.ByQuery),
	)
	cancelFocus()
	time.Sleep(300 * time.Millisecond)

	// 逐行键盘输入, 行间 Shift+Enter 换行
	lines := strings.Split(messageText, "\n")
	for i, line := range lines {
		if i > 0 {
			shiftCtx, cancelShift := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(shiftCtx, chromedp.KeyEvent("\r", chromedp.KeyModifiers(input.ModifierShift)))
			cancelShift()
		}
		if line != "" {
			lineCtx, cancelLine := context.WithTimeout(ctx, 10*time.Second)
			if err := chromedp.Run(lineCtx, chromedp.SendKeys(foundSelector, line, chromedp.ByQuery)); err != nil {
				cancelLine()
				return fmt.Errorf("FB_MSG4 键盘输入消息失败: %v", err)
			}
			cancelLine()
		}
	}

	// 回读校验, 确认内容确实写入 Lexical 编辑器
	var finalText string
	finalCtx, cancelFinal := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(finalCtx, chromedp.Evaluate(readJS, &finalText))
	cancelFinal()
	if finalText == "" {
		return errors.New("FB_MSG4 消息内容未能写入输入框")
	}
	m.logger.Print("FB_MSG4", "已填写消息内容: "+finalText)
	return nil
}

// verifySent 校验发送成功: 回车后轮询读取输入框, 成功发送后 Facebook 会清空输入框。
// (参考 Instagram 的 verifySent 思路; 因 FB 会话消息 DOM 结构不稳定, 采用"输入框清空"这一更通用的判定。)
func (m *facebookMessenger) verifySent(ctx context.Context, content string) error {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	js := `(function(){
		var sels = ["div[role='textbox'][aria-label^='Write to']", "div[data-lexical-editor='true'][aria-placeholder='Aa']", "div[role='textbox'][contenteditable='true']"];
		for(var i=0;i<sels.length;i++){
			var el = document.querySelector(sels[i]);
			if(el){ return (el.innerText || el.textContent || '').trim(); }
		}
		return '__NO_INPUT__';
	})()`

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var txt string
		evalCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(evalCtx, chromedp.Evaluate(js, &txt))
		cancel()

		if txt != "__NO_INPUT__" && txt == "" {
			m.logger.Print("FB_MSG5", "消息已发送成功(输入框已清空)")
			return nil
		}

		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return fmt.Errorf("验证发送结果时上下文超时: %v", ctx.Err())
		}
	}
	return errors.New("FB_MSG5 20秒内未确认消息发送成功(输入框未清空)")
}
