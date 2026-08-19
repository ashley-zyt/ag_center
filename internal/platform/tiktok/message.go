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
// TODO(校准): 以下收件箱相关选择器为初始候选, 需在真实 TikTok 收件箱页面实测后校准。
// 校准时只需修改这些数组, 无需改动流程代码。

// ttConversationItemSelectors 收件箱左侧会话列表项候选选择器(按优先级)
var ttConversationItemSelectors = []string{
	`div[data-e2e="dm-link"]`,
	`div[data-e2e="chat-list-item"]`,
	`div[class*="DivItemContainer"] a[href*="/messages"]`,
	`div[data-e2e="dm-inbox-list"] div[role="listitem"]`,
}

// ttMessageItemSelectors 会话内单条消息气泡候选选择器(按优先级)
var ttMessageItemSelectors = []string{
	`div[data-e2e="dm-chat-item"]`,
	`div[class*="DivMessageCard"]`,
	`div[class*="MessageItem"]`,
	`div[class*="message-list"] > div`,
}

// ttMessageContainerSelectors 会话消息区容器候选选择器(用于判定会话已打开)
var ttMessageContainerSelectors = []string{
	`div[data-e2e="dm-chat"]`,
	`div[class*="ChatList"]`,
	`div[class*="message-list"]`,
}

// ─────────────────────────── 入口函数 ───────────────────────────

// SendTikTokMessage 批量主动私信入口: 打开对方主页 -> 点击发消息 -> 输入发送
func SendTikTokMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &tiktokMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// FetchTikTokConversations 会话列表拉取入口: 收件箱列表 + 每个会话的最新消息
func FetchTikTokConversations(ctx context.Context, logger *logx.Logger, opts message.FetchOptions) (message.FetchConversationsResult, error) {
	m := &tiktokMessenger{logger: logger}
	return message.RunFetchConversations(ctx, logger, m, opts), nil
}

// tiktokMessenger 实现 message.MessengerActions
type tiktokMessenger struct {
	logger *logx.Logger
}

func (t *tiktokMessenger) Tag() string      { return "TT_MSG" }
func (t *tiktokMessenger) InboxURL() string { return "https://www.tiktok.com/messages" }

// CheckLogin 检测当前页面登录态: DOM探针找登录按钮 / 风控文案
// 探针使用完整文案'Log in to TikTok'而非泛化的'Log in', 避免页面上"Log in to comment"等链接造成误判
func (t *tiktokMessenger) CheckLogin(ctx context.Context) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var loginNodes []*cdp.Node
	_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[contains(text(), 'Log in to TikTok')]`, &loginNodes, chromedp.BySearch))
	if len(loginNodes) > 0 {
		return message.StatusNotLoggedIn, nil
	}

	var riskNodes []*cdp.Node
	_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[contains(text(), 'Something went wrong') or contains(text(), '出现了一点问题') or contains(text(), 'We regret to inform you that we have discontinued operating TikTok in Hong Kong.')]`, &riskNodes, chromedp.BySearch))
	if len(riskNodes) > 0 {
		return message.StatusAbnormal, nil
	}
	return message.StatusLoggedIn, nil
}

// OpenTargetProfile 导航到对方主页
func (t *tiktokMessenger) OpenTargetProfile(ctx context.Context, task message.SendTask) error {
	if err := chromedp.Run(ctx, chromedp.Navigate(task.TargetURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	t.logger.Print("TT_MSG2", "已打开目标用户主页: "+task.TargetURL)
	return nil
}

// OpenConversationFromProfile 在对方主页点击"Message/发消息"按钮进入会话
func (t *tiktokMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	return clickMessageButton(ctx, t.logger)
}

// SendInConversation 在已打开的会话中输入内容并点击发送
func (t *tiktokMessenger) SendInConversation(ctx context.Context, content string) error {
	if err := waitAndFillMessage(ctx, t.logger, content); err != nil {
		return err
	}
	if err := clickSendButton(ctx, t.logger); err != nil {
		return err
	}
	// TODO(校准): 发送后可回读输入框是否清空/消息列表是否新增最后一条, 进一步确认发送成功
	time.Sleep(2 * time.Second)
	return nil
}

// FetchConversationList 解析收件箱会话列表(带30秒轮询等待列表渲染)
func (t *tiktokMessenger) FetchConversationList(ctx context.Context, opts message.FetchOptions) ([]message.Conversation, error) {
	sels, _ := json.Marshal(ttConversationItemSelectors)
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
			var txt = (it.innerText || '').split('\n').map(function(x){ return x.trim(); }).filter(Boolean);
			var a = it.querySelector('a[href]') || (it.tagName === 'A' ? it : null);
			var href = a ? a.href : '';
			var unread = false;
			try { unread = /unread|未读/i.test(it.innerHTML.slice(0, 2000)); } catch (e) {}
			return {
				conversation_id: href || ('idx:' + idx),
				partner_name: txt[0] || '',
				last_message: txt.length > 2 ? txt[1] || '' : '',
				last_message_at: txt[txt.length - 1] || '',
				partner_url: href,
				unread: unread
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

// OpenConversation 从收件箱打开指定会话: 优先按会话链接导航, 否则按对方名称点击列表项
func (t *tiktokMessenger) OpenConversation(ctx context.Context, conv message.Conversation) error {
	if strings.HasPrefix(conv.ConversationID, "http") {
		if err := chromedp.Run(ctx, chromedp.Navigate(conv.ConversationID), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return err
		}
	} else {
		sels, _ := json.Marshal(ttConversationItemSelectors)
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

	// 等待消息区渲染
	containerSel := strings.Join(ttMessageContainerSelectors, ", ")
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = chromedp.Run(waitCtx, chromedp.WaitVisible(containerSel, chromedp.ByQuery))
	time.Sleep(2 * time.Second)
	return nil
}

// FetchConversationMessages 解析当前会话的最新消息(时间正序: 旧->新)
func (t *tiktokMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	msgSels, _ := json.Marshal(ttMessageItemSelectors)
	containerSel := strings.Join(ttMessageContainerSelectors, ", ")
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
			var cls = '';
			try { cls = it.className + ' ' + (it.parentElement ? it.parentElement.className : ''); } catch (e) {}
			var outgoing = /self|right|mine|outgoing/i.test(cls);
			var lines = (it.innerText || '').split('\n').map(function(x){ return x.trim(); }).filter(Boolean);
			var content = lines.join(' ');
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

// ─────────────────────────── 主页发消息辅助 ───────────────────────────

// clickMessageButton 查找并点击"Message"或"发消息"按钮
func clickMessageButton(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT_MSG3", "查找Message按钮")
	candidates := []string{
		`//button[contains(., 'Message') or contains(., '发消息') or contains(., '私信')]`,
		`//div[@role='button' and (contains(., 'Message') or contains(., '发消息') or contains(., '私信'))]`,
		`//a[contains(@href, '/direct') or contains(@href, '/message')]`,
		`//button[@data-e2e='message-button']`,
		`//div[@data-e2e='message-button']`,
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
				err := chromedp.Run(clickCtx,
					chromedp.ScrollIntoView(sel, chromedp.BySearch),
					chromedp.WaitVisible(sel, chromedp.BySearch),
					chromedp.Click(sel, chromedp.BySearch),
				)
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
		`//textarea[@placeholder='Message' or @placeholder='发消息' or @placeholder='私信']`,
		`//div[@role='textbox' and @contenteditable='true']`,
		`//input[@type='text' and (@placeholder='Message' or @placeholder='发消息')]`,
		`//div[data-e2e='message-input']`,
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
	logger.Print("TT_MSG4", "填写消息内容")

	// 使用JavaScript插入文本，兼容contenteditable元素
	var ok bool
	js := fmt.Sprintf(`(function(msg){
		var el = document.evaluate(%q, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue;
		if(!el) {
			el = document.querySelector(%q);
		}
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
	})(%q)`, foundSelector, strings.TrimPrefix(foundSelector, "//"), messageText)

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
