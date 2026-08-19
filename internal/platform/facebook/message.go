package facebook

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
// TODO(校准): 以下选择器为初始候选, 需在真实页面实测后校准。校准时只改这些数组。
// FB 会话链接 /messages/t/<id> 稳定可导航, 会话定位优先走链接。

// fbMessageButtonSelectors 对方主页"发消息"按钮候选
var fbMessageButtonSelectors = []string{
	`//a[contains(@href, '/messages/t/')]`,
	`//span[text()='Message' or text()='发消息']/ancestor::div[@role='button'][1]`,
	`//span[text()='Message' or text()='发消息']/ancestor::a[1]`,
	`//div[@role='button'][@aria-label='Message' or @aria-label='发消息']`,
}

// fbMessageInputSelectors 消息输入框候选
var fbMessageInputSelectors = []string{
	`//div[@role='textbox' and @contenteditable='true']`,
	`//div[@role='textbox'][contains(@aria-label, 'message') or contains(@aria-label, '消息') or @aria-label='Aa']`,
}

// fbSendButtonSelectors 发送按钮候选
var fbSendButtonSelectors = []string{
	`//div[@role='button'][@aria-label='Press Enter to send' or @aria-label='按 Enter 键发送']`,
	`//div[@aria-label='Press Enter to send']/ancestor::div[@role='button'][1]`,
	`//span[text()='发送' or text()='Send']/ancestor::div[@role='button'][1]`,
}

// fbConversationItemSelectors 收件箱会话列表项候选
var fbConversationItemSelectors = []string{
	`a[href*="/messages/t/"][role="link"]`,
	`a[href*="/messages/t/"]`,
	`div[role="row"][aria-label]`,
}

// fbMessageContainerSelectors 会话消息区容器候选(判定会话已打开)
var fbMessageContainerSelectors = []string{
	`div[role="main"]`,
	`div[aria-label="消息"], div[aria-label="Chats"], div[aria-label="Messages"]`,
}

// fbMessageItemSelectors 会话内单条消息候选
var fbMessageItemSelectors = []string{
	`div[role="row"]`,
	`div[data-testid="message"]`,
}

// ─────────────────────────── 入口函数 ───────────────────────────

// SendFacebookMessage 批量主动私信入口
func SendFacebookMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &facebookMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// FetchFacebookConversations 会话列表拉取入口
func FetchFacebookConversations(ctx context.Context, logger *logx.Logger, opts message.FetchOptions) (message.FetchConversationsResult, error) {
	m := &facebookMessenger{logger: logger}
	return message.RunFetchConversations(ctx, logger, m, opts), nil
}

// facebookMessenger 实现 message.MessengerActions
type facebookMessenger struct {
	logger *logx.Logger
}

func (m *facebookMessenger) Tag() string      { return "FB_MSG" }
func (m *facebookMessenger) InboxURL() string { return "https://www.facebook.com/messages/" }

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
	if err := chromedp.Run(ctx, chromedp.Navigate(task.TargetURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	m.logger.Print("FB_MSG2", "已打开目标用户主页: "+task.TargetURL)
	return nil
}

// OpenConversationFromProfile 在对方主页点击"发消息"按钮进入会话
func (m *facebookMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	return m.clickMessageButton(ctx)
}

// SendInConversation 输入内容并点击发送
func (m *facebookMessenger) SendInConversation(ctx context.Context, content string) error {
	if err := m.waitAndFillMessage(ctx, content); err != nil {
		return err
	}
	if err := m.clickSendButton(ctx); err != nil {
		return err
	}
	// TODO(校准): 发送后可回读输入框是否清空进一步确认
	time.Sleep(2 * time.Second)
	return nil
}

// FetchConversationList 解析收件箱会话列表(带30秒轮询等待列表渲染)
func (m *facebookMessenger) FetchConversationList(ctx context.Context, opts message.FetchOptions) ([]message.Conversation, error) {
	sels, _ := json.Marshal(fbConversationItemSelectors)
	js := fmt.Sprintf(`(function(){
		var sels = %s;
		var items = [];
		var seen = {};
		for (var i = 0; i < sels.length; i++) {
			try {
				var found = document.querySelectorAll(sels[i]);
				for (var k = 0; k < found.length; k++) {
					var href = found[k].href || '';
					if (href && seen[href]) continue;
					if (href) seen[href] = true;
					items.push(found[k]);
				}
				if (items.length) break;
			} catch (e) {}
		}
		if (!items.length) return "[]";
		var out = items.map(function(it, idx){
			var lines = (it.innerText || (it.getAttribute('aria-label') || '')).split('\n').map(function(x){ return x.trim(); }).filter(Boolean);
			var a = it.querySelector('a[href]') || (it.tagName === 'A' ? it : null);
			var href = a ? a.href : (it.href || '');
			return {
				conversation_id: href || ('idx:' + idx),
				partner_name: lines[0] || '',
				last_message: lines.length > 2 ? lines[1] || '' : '',
				last_message_at: lines[lines.length - 1] || '',
				partner_url: '',
				unread: false
			};
		});
		return JSON.stringify(out);
	})()`, string(sels))

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var raw string
		evalCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(evalCtx, chromedp.Evaluate(js, &raw))
		cancel()
		if err == nil && raw != "" && raw != "[]" {
			var convs []message.Conversation
			if err := json.Unmarshal([]byte(raw), &convs); err != nil {
				return nil, fmt.Errorf("解析会话列表JSON失败: %v", err)
			}
			return convs, nil
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("30秒内未在收件箱解析到会话列表(选择器可能需校准)")
}

// OpenConversation 从收件箱打开指定会话: 优先按会话链接导航, 否则按对方名称点击
func (m *facebookMessenger) OpenConversation(ctx context.Context, conv message.Conversation) error {
	if strings.HasPrefix(conv.ConversationID, "http") {
		if err := chromedp.Run(ctx, chromedp.Navigate(conv.ConversationID), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return err
		}
	} else {
		sels, _ := json.Marshal(fbConversationItemSelectors)
		js := fmt.Sprintf(`(function(sels, name){
			for (var i = 0; i < sels.length; i++) {
				var items;
				try { items = document.querySelectorAll(sels[i]); } catch (e) { continue; }
				for (var j = 0; j < items.length; j++) {
					var txt = (items[j].innerText || '').trim();
					if (name && txt.indexOf(name) !== -1) {
						items[j].click();
						return true;
					}
				}
			}
			return false;
		})(%s, %q)`, string(sels), conv.PartnerName)
		var clicked bool
		clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(clickCtx, chromedp.Evaluate(js, &clicked))
		cancel()
		if err != nil {
			return err
		}
		if !clicked {
			return fmt.Errorf("未找到会话列表项: %s", conv.PartnerName)
		}
	}

	containerSel := strings.Join(fbMessageContainerSelectors, ", ")
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = chromedp.Run(waitCtx, chromedp.WaitVisible(containerSel, chromedp.ByQuery))
	time.Sleep(2 * time.Second)
	return nil
}

// FetchConversationMessages 解析当前会话的最新消息(时间正序)
// TODO(校准): direction 依据消息行 aria-label 是否以 "You"/"你" 开头(自己发出), 实测后校准
func (m *facebookMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	msgSels, _ := json.Marshal(fbMessageItemSelectors)
	containerSel := strings.Join(fbMessageContainerSelectors, ", ")
	js := fmt.Sprintf(`(function(contSel, msgSels){
		var cont = document.querySelector(contSel);
		if (!cont) return "[]";
		var items = [];
		for (var i = 0; i < msgSels.length; i++) {
			try {
				var found = cont.querySelectorAll(msgSels[i]);
				if (found && found.length) { items = Array.prototype.slice.call(found); break; }
			} catch (e) {}
		}
		if (!items.length) return "[]";
		var out = items.map(function(it){
			var label = it.getAttribute('aria-label') || '';
			var outgoing = /^(你|You)([\\s:：]|$)/.test(label.trim());
			var content = (it.innerText || label).split('\\n').map(function(x){ return x.trim(); }).filter(Boolean).join(' ');
			var sentAt = '';
			var timeMatch = label.match(/\\d{1,2}:\\d{2}/);
			if (timeMatch) sentAt = timeMatch[0];
			return {
				direction: outgoing ? 'outgoing' : 'incoming',
				content: content,
				sent_at: sentAt
			};
		});
		return JSON.stringify(out);
	})(%q, %s)`, containerSel, string(msgSels))

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

// waitAndFillMessage 等待消息输入框并填写内容
func (m *facebookMessenger) waitAndFillMessage(ctx context.Context, messageText string) error {
	m.logger.Print("FB_MSG4", "等待消息输入框")
	var foundSelector string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range fbMessageInputSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
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
		return errors.New("FB_MSG4 60秒内未找到消息输入框")
	}

	m.logger.Print("FB_MSG4", "找到消息输入框: "+foundSelector)
	var ok bool
	js := fmt.Sprintf(`(function(msg){
		var el = document.evaluate(%q, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue;
		if(!el) return false;
		el.focus();
		try{
			var sel = window.getSelection();
			if(sel) {
				sel.removeAllRanges();
				var r = document.createRange();
				r.selectNodeContents(el);
				sel.addRange(r);
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
	})(%q)`, foundSelector, messageText)

	evalCtx, cancelEval := context.WithTimeout(ctx, 10*time.Second)
	if err := chromedp.Run(evalCtx, chromedp.Evaluate(js, &ok)); err != nil {
		cancelEval()
		return fmt.Errorf("FB_MSG4 注入消息文本失败: %v", err)
	}
	cancelEval()

	if !ok {
		sendCtx, cancelSend := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(sendCtx, chromedp.SendKeys(foundSelector, messageText, chromedp.BySearch))
		cancelSend()
		if err != nil {
			return fmt.Errorf("FB_MSG4 键盘输入消息失败: %v", err)
		}
		m.logger.Print("FB_MSG4", "已使用键盘输入消息")
	} else {
		m.logger.Print("FB_MSG4", "已填写消息内容")
	}
	return nil
}

// clickSendButton 查找并点击发送按钮
func (m *facebookMessenger) clickSendButton(ctx context.Context) error {
	m.logger.Print("FB_MSG5", "查找发送按钮")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range fbSendButtonSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			cancel()

			if len(nodes) > 0 {
				m.logger.Print("FB_MSG5", "找到发送按钮: "+sel)
				clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
				err := chromedp.Run(clickCtx,
					chromedp.ScrollIntoView(sel, chromedp.BySearch),
					chromedp.WaitVisible(sel, chromedp.BySearch),
					chromedp.Click(sel, chromedp.BySearch),
				)
				cancelClick()
				if err == nil {
					m.logger.Print("FB_MSG5", "已点击发送按钮")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("FB_MSG5 60秒内未找到发送按钮")
}
