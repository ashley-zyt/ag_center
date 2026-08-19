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
// TODO(校准): 以下选择器为初始候选, 需在真实页面实测后校准。校准时只改这些数组。

// igMessageButtonSelectors 对方主页"发消息"按钮候选(按优先级)
var igMessageButtonSelectors = []string{
	`//button[normalize-space(.)='Message' or normalize-space(.)='发消息']`,
	`//div[@role='button'][normalize-space(.)='Message' or normalize-space(.)='发消息']`,
	`//a[contains(@href, '/direct/t/')]`,
	`//button[contains(., 'Message') or contains(., '发消息')]`,
}

// igMessageInputSelectors 消息输入框候选
var igMessageInputSelectors = []string{
	`//textarea[contains(@placeholder, 'Message') or contains(@placeholder, '发消息') or contains(@placeholder, '消息')]`,
	`//div[@role='textbox' and @contenteditable='true']`,
	`//textarea[@placeholder='Aa']`,
}

// igSendButtonSelectors 发送按钮候选
var igSendButtonSelectors = []string{
	`//button[@type='submit']`,
	`//div[@role='button'][@aria-label='发送' or @aria-label='Send']`,
	`//button[contains(., 'Send') or contains(., '发送')]`,
}

// igConversationItemSelectors 收件箱会话列表项候选
var igConversationItemSelectors = []string{
	`a[href*="/direct/t/"]`,
	`div[role="listitem"]`,
	`div[aria-label="Chats"] a[href*="/direct/t/"]`,
}

// igMessageContainerSelectors 会话消息区容器候选(判定会话已打开)
var igMessageContainerSelectors = []string{
	`div[role="grid"]`,
	`div[data-scope="messages_tab"]`,
	`div[aria-label="消息"], div[aria-label="Messages"]`,
}

// igMessageItemSelectors 会话内单条消息气泡候选
var igMessageItemSelectors = []string{
	`div[role="row"]`,
	`div[role="gridcell"]`,
}

// ─────────────────────────── 入口函数 ───────────────────────────

// SendInstagramMessage 批量主动私信入口
func SendInstagramMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &instagramMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// FetchInstagramConversations 会话列表拉取入口
func FetchInstagramConversations(ctx context.Context, logger *logx.Logger, opts message.FetchOptions) (message.FetchConversationsResult, error) {
	m := &instagramMessenger{logger: logger}
	return message.RunFetchConversations(ctx, logger, m, opts), nil
}

// instagramMessenger 实现 message.MessengerActions
type instagramMessenger struct {
	logger *logx.Logger
}

func (m *instagramMessenger) Tag() string { return "IG_MSG" }

// InboxURL 返回主页而非直接收件箱地址: IG 直接访问功能子页面(如 /reels/)曾触发
// "Something went wrong", 经验上从主页入口进入更稳, 收件箱入口点击在 ensureInbox 中处理
func (m *instagramMessenger) InboxURL() string { return "https://www.instagram.com/" }

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

// OpenConversationFromProfile 在对方主页点击"Message"按钮进入会话
func (m *instagramMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	return m.clickMessageButton(ctx)
}

// SendInConversation 输入内容并点击发送
func (m *instagramMessenger) SendInConversation(ctx context.Context, content string) error {
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

// FetchConversationList 解析收件箱会话列表(先经主页入口进入收件箱)
func (m *instagramMessenger) FetchConversationList(ctx context.Context, opts message.FetchOptions) ([]message.Conversation, error) {
	if err := m.ensureInbox(ctx); err != nil {
		return nil, err
	}

	sels, _ := json.Marshal(igConversationItemSelectors)
	js := fmt.Sprintf(`(function(){
		var sels = %s;
		var items = [];
		for (var i = 0; i < sels.length; i++) {
			try {
				var found = document.querySelectorAll(sels[i]);
				if (found && found.length) { items = Array.prototype.slice.call(found); break; }
			} catch (e) {}
		}
		if (!items.length) return "[]";
		var out = items.map(function(it, idx){
			var lines = (it.innerText || '').split('\n').map(function(x){ return x.trim(); }).filter(Boolean);
			var a = it.querySelector('a[href]') || (it.tagName === 'A' ? it : null);
			var href = a ? a.href : '';
			return {
				conversation_id: href || ('idx:' + idx),
				partner_name: lines[0] || '',
				last_message: lines[1] || '',
				last_message_at: lines[lines.length - 1] || '',
				partner_url: href,
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
func (m *instagramMessenger) OpenConversation(ctx context.Context, conv message.Conversation) error {
	if strings.HasPrefix(conv.ConversationID, "http") {
		if err := chromedp.Run(ctx, chromedp.Navigate(conv.ConversationID), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return err
		}
	} else {
		sels, _ := json.Marshal(igConversationItemSelectors)
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

	containerSel := strings.Join(igMessageContainerSelectors, ", ")
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = chromedp.Run(waitCtx, chromedp.WaitVisible(containerSel, chromedp.ByQuery))
	time.Sleep(2 * time.Second)
	return nil
}

// FetchConversationMessages 解析当前会话的最新消息(时间正序)
// TODO(校准): outgoing 判定目前用"气泡内无头像即自己发出"的启发式, 群聊/图片消息场景需校准
func (m *instagramMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	msgSels, _ := json.Marshal(igMessageItemSelectors)
	containerSel := strings.Join(igMessageContainerSelectors, ", ")
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
			var outgoing = !it.querySelector('img');
			var content = (it.innerText || '').split('\n').map(function(x){ return x.trim(); }).filter(Boolean).join(' ');
			var sentAt = '';
			var timeMatch = (it.getAttribute('aria-label') || '').match(/\d{1,2}:\d{2}/);
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

// ensureInbox 确保当前位于 /direct/ 收件箱; 不在则从主页点击私信入口进入
func (m *instagramMessenger) ensureInbox(ctx context.Context) error {
	var loc string
	if err := chromedp.Run(ctx, chromedp.Location(&loc)); err != nil {
		return err
	}
	if strings.Contains(loc, "/direct/") {
		return nil
	}

	m.logger.Print("IG_MSG3", "当前不在收件箱, 尝试从主页入口进入")
	if !strings.HasPrefix(loc, "https://www.instagram.com/") {
		if err := chromedp.Run(ctx, chromedp.Navigate("https://www.instagram.com/"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	}

	// 点击私信图标: 优先找 /direct/inbox 链接, 否则从 svg[aria-label] 向上找可点击容器
	// (经验: 直接 click svg 会报 'click is not a function', 需点击其外层容器)
	var via string
	clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	js := `(function(){
		var link = document.querySelector('a[href="/direct/inbox/"]') || document.querySelector('a[href*="/direct/inbox"]');
		if (link) { link.click(); return 'link'; }
		var svgs = document.querySelectorAll('svg[aria-label="Direct"], svg[aria-label="私信"], svg[aria-label="Messenger"]');
		for (var i = 0; i < svgs.length; i++) {
			var t = svgs[i].parentElement;
			while (t && t !== document.body) {
				if (t.tagName === 'A' || t.getAttribute('role') === 'button' || t.getAttribute('role') === 'link') { t.click(); return 'svg'; }
				t = t.parentElement;
			}
		}
		return '';
	})()`
	if err := chromedp.Run(clickCtx, chromedp.Evaluate(js, &via)); err != nil {
		return err
	}
	if via == "" {
		return errors.New("IG_MSG3 未找到收件箱入口(选择器需校准)")
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(ctx, chromedp.Location(&loc)); err == nil && strings.Contains(loc, "/direct/") {
			time.Sleep(3 * time.Second)
			return nil
		}
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("IG_MSG3 点击收件箱入口后未跳转到 /direct/")
}

// clickMessageButton 查找并点击对方主页的"Message/发消息"按钮
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
	return errors.New("IG_MSG3 60秒内未找到Message按钮(对方可能关闭了私信)")
}

// waitAndFillMessage 等待消息输入框并填写内容
func (m *instagramMessenger) waitAndFillMessage(ctx context.Context, messageText string) error {
	m.logger.Print("IG_MSG4", "等待消息输入框")
	var foundSelector string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range igMessageInputSelectors {
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
		return errors.New("IG_MSG4 60秒内未找到消息输入框")
	}

	m.logger.Print("IG_MSG4", "找到消息输入框: "+foundSelector)
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
		return fmt.Errorf("IG_MSG4 注入消息文本失败: %v", err)
	}
	cancelEval()

	if !ok {
		sendCtx, cancelSend := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(sendCtx, chromedp.SendKeys(foundSelector, messageText, chromedp.BySearch))
		cancelSend()
		if err != nil {
			return fmt.Errorf("IG_MSG4 键盘输入消息失败: %v", err)
		}
		m.logger.Print("IG_MSG4", "已使用键盘输入消息")
	} else {
		m.logger.Print("IG_MSG4", "已填写消息内容")
	}
	return nil
}

// clickSendButton 查找并点击发送按钮
func (m *instagramMessenger) clickSendButton(ctx context.Context) error {
	m.logger.Print("IG_MSG5", "查找发送按钮")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range igSendButtonSelectors {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			cancel()

			if len(nodes) > 0 {
				m.logger.Print("IG_MSG5", "找到发送按钮: "+sel)
				clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
				err := chromedp.Run(clickCtx,
					chromedp.ScrollIntoView(sel, chromedp.BySearch),
					chromedp.WaitVisible(sel, chromedp.BySearch),
					chromedp.Click(sel, chromedp.BySearch),
				)
				cancelClick()
				if err == nil {
					m.logger.Print("IG_MSG5", "已点击发送按钮")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("IG_MSG5 60秒内未找到发送按钮")
}
