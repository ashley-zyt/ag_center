package twitter

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

// ─────────────────────────── 常量与选择器 ───────────────────────────
// 选择器均提取自真实页面 dump(2026-08), X 新版聊天界面(/i/chat)以 data-testid 为主。

const (
	// twChatURL X 新版私信页面入口
	twChatURL = "https://x.com/i/chat"
	// twDefaultPasscode Passcode 未传入时的默认值
	twDefaultPasscode = "1472"
)

// 发送流程选择器: 全部为 data-testid 结构性定位, 不依赖页面文字/aria-label 等本地化文本。
// 唯一性已在各 dump 中逐一验证(均只出现1次), 列表类元素通过唯一容器级联限定后取首项。
const (
	selPinSetupBtn     = `[data-testid="pin-onboarding-setup-now"]`       // Create Passcode 按钮(dump1 唯一)
	selPinTitle        = `[data-testid="pin-title"]`                      // 密码界面标题(dump2/4 唯一, 仅日志读取)
	selPinContainer    = `[data-testid="pin-code-input-container"]`       // 4格密码输入容器(dump2/4 唯一)
	selPinFirstInput   = `[data-testid="pin-code-input-container"] input` // 密码第一格(容器内第1个input)
	selInboxPanel      = `div[data-testid="dm-inbox-panel"]`              // 私信收件箱面板(dump6 唯一)
	selNewChatBtn      = `[data-testid="dm-new-chat-button"]`             // New message 按钮(dump6 唯一)
	selSearchInput     = `input[data-testid="new-dm-search-input"]`       // 联系人搜索框(dump8 唯一)
	selSuggestList     = `[data-testid="new-dm-suggestions-list"]`        // 搜索结果列表容器(dump10 唯一)
	selEmptyInbox      = `[data-testid="dm-empty-inbox"]`                 // 空收件箱占位(dump6 唯一)
	selComposer        = `textarea[data-testid="dm-composer-textarea"]`   // 消息输入框(dump12 唯一)
	selMessageScroller = `[data-testid="dm-message-scroller"]`            // 聊天记录容器(dump12 唯一)
)

// twConversationItemSelectors 收件箱会话列表项候选: 全部级联限定在唯一收件箱面板内,
// 避免命中导航等页面上其他区域的元素(收件箱为空时无法从 dump 确认结构, 待实测校准)。
var twConversationItemSelectors = []string{
	`div[data-testid="dm-inbox-panel"] [role="listitem"]`,
	`div[data-testid="dm-inbox-panel"] div[data-testid^="conversation"]`,
	`div[data-testid="dm-inbox-panel"] a[href*="/messages/"]`,
}

// twMessageContainerSelectors 会话消息区容器候选
var twMessageContainerSelectors = []string{
	selMessageScroller,
	`div[data-testid="dm-message-list-container"]`,
	`div[data-testid="DmActivityViewport"]`,
}

// twMessageItemSelectors 会话内单条消息候选
var twMessageItemSelectors = []string{
	`div[data-testid="dm-message-scroller"] [data-index]`,
	`div[data-testid="messageEntry"]`,
	`div[data-testid="DmActivityItem"]`,
}

// ─────────────────────────── 入口函数 ───────────────────────────

// SendTwitterMessage 批量主动私信入口:
// 私信主页(/i/chat) -> Passcode验证 -> New message -> 搜索账号 -> 回车发送 -> 验证成功
func SendTwitterMessage(ctx context.Context, logger *logx.Logger, tasks []message.SendTask) (message.SendResult, error) {
	m := &twitterMessenger{logger: logger}
	return message.RunSend(ctx, logger, m, tasks), nil
}

// FetchTwitterConversations 会话列表拉取入口
func FetchTwitterConversations(ctx context.Context, logger *logx.Logger, opts message.FetchOptions) (message.FetchConversationsResult, error) {
	m := &twitterMessenger{logger: logger}
	return message.RunFetchConversations(ctx, logger, m, opts), nil
}

// twitterMessenger 实现 message.MessengerActions
type twitterMessenger struct {
	logger *logx.Logger
}

func (m *twitterMessenger) Tag() string      { return "TW_MSG" }
func (m *twitterMessenger) InboxURL() string { return twChatURL }

// CheckLogin 登录态检测: 未登录时访问私信页会被重定向到 /login 或渲染登录表单
func (m *twitterMessenger) CheckLogin(ctx context.Context) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var loc string
	if err := chromedp.Run(checkCtx, chromedp.Location(&loc)); err == nil {
		if strings.Contains(loc, "x.com/login") || strings.Contains(loc, "twitter.com/login") {
			return message.StatusNotLoggedIn, nil
		}
	}

	var status string
	js := `(function(){
		var loginForm = document.querySelector('form[action="/login"], form[action*="/account/login"], input[autocomplete="username"]');
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

// OpenTargetProfile 打开私信主页并处理 Passcode 验证拦截(X 为搜索型平台, 不访问对方主页)
func (m *twitterMessenger) OpenTargetProfile(ctx context.Context, task message.SendTask) error {
	m.logger.Print("TW_MSG2", "打开私信主页: "+twChatURL)
	if err := chromedp.Run(ctx, chromedp.Navigate(twChatURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)

	// 未登录会被重定向到登录页
	var loc string
	_ = chromedp.Run(ctx, chromedp.Location(&loc))
	if strings.Contains(loc, "/login") {
		return errors.New("账号未登录(访问私信页被重定向到登录页)")
	}

	// 处理 Passcode 验证拦截
	if err := m.handlePasscode(ctx, task.Passcode); err != nil {
		return err
	}

	// 等待收件箱面板出现
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := chromedp.Run(waitCtx, chromedp.WaitVisible(selInboxPanel, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("私信主页面未就绪: %v", err)
	}
	m.logger.Print("TW_MSG2", "已进入私信主页面")
	return nil
}

// handlePasscode 处理 X 私信 Passcode 验证拦截, 三种形态:
// ① 首次设置引导页(Create Passcode 按钮) ② 已有密码直接要求输入 ③ 无拦截直接通过。
// 流程: 点击Create Passcode(如出现) -> 输入4位密码 -> 回车 -> 二次确认输入 -> 等待跳转回私信主页。
func (m *twitterMessenger) handlePasscode(ctx context.Context, passcode string) error {
	if passcode == "" {
		passcode = twDefaultPasscode
	}

	// ① 检测首次设置引导页
	var setupBtns []*cdp.Node
	detectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(detectCtx, chromedp.Nodes(selPinSetupBtn, &setupBtns, chromedp.ByQuery))
	cancel()

	if len(setupBtns) > 0 {
		m.logger.Print("TW_MSG2", "检测到 Passcode 首次设置引导, 点击 Create Passcode")
		clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(clickCtx, chromedp.Click(selPinSetupBtn, chromedp.ByQuery))
		cancelClick()
		if err != nil {
			return fmt.Errorf("点击 Create Passcode 按钮失败: %v", err)
		}
		time.Sleep(2 * time.Second)
	}

	// ② 检测密码输入界面(创建/确认/输入共用同一容器)
	if !m.pinInputVisible(ctx) {
		if len(setupBtns) > 0 {
			return errors.New("点击 Create Passcode 后未出现密码输入界面")
		}
		return nil // 无拦截
	}

	var pinTitle string
	_ = chromedp.Run(ctx, chromedp.Text(selPinTitle, &pinTitle, chromedp.ByQuery))
	m.logger.Print("TW_MSG2", "进入 Passcode 输入界面: "+strings.TrimSpace(pinTitle))

	// 首次输入 + 回车提交
	if err := m.fillPasscode(ctx, passcode); err != nil {
		return err
	}
	m.logger.Print("TW_MSG2", "已输入 Passcode")
	if err := m.submitPasscode(ctx); err != nil {
		return err
	}
	if err := m.waitPinGone(ctx, 15*time.Second); err != nil {
		return err
	}

	// ③ 二次确认界面(Confirm Passcode), 输入完成后系统自动跳转回私信主页面
	if m.pinInputVisible(ctx) {
		var confirmTitle string
		_ = chromedp.Run(ctx, chromedp.Text(selPinTitle, &confirmTitle, chromedp.ByQuery))
		m.logger.Print("TW_MSG2", "进入 Passcode 二次确认界面: "+strings.TrimSpace(confirmTitle))
		if err := m.fillPasscode(ctx, passcode); err != nil {
			return err
		}
		m.logger.Print("TW_MSG2", "已二次输入 Passcode, 等待自动跳转回私信主页面")
		if err := m.submitPasscode(ctx); err != nil {
			return err
		}
		if err := m.waitPinGone(ctx, 15*time.Second); err != nil {
			return err
		}
	}
	m.logger.Print("TW_MSG2", "Passcode 验证通过")
	return nil
}

// pinInputVisible 密码输入容器是否可见
func (m *twitterMessenger) pinInputVisible(ctx context.Context) bool {
	var nodes []*cdp.Node
	detectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = chromedp.Run(detectCtx, chromedp.Nodes(selPinContainer, &nodes, chromedp.ByQuery))
	return len(nodes) > 0
}

// fillPasscode 在4格密码输入框中输入密码:
// 点击第一格获得焦点后连续输入, PIN组件每位输入后自动聚焦下一格;
// 若未自动进格(回读校验失败), 兜底按容器内序号逐格点击输入(容器唯一, 子div顺序即密码位序)。
func (m *twitterMessenger) fillPasscode(ctx context.Context, passcode string) error {
	clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
	err := chromedp.Run(clickCtx, chromedp.Click(selPinFirstInput, chromedp.ByQuery))
	cancelClick()
	if err != nil {
		return fmt.Errorf("点击密码输入框失败: %v", err)
	}

	typeCtx, cancelType := context.WithTimeout(ctx, 10*time.Second)
	err = chromedp.Run(typeCtx, chromedp.SendKeys(selPinFirstInput, passcode, chromedp.ByQuery))
	cancelType()
	if err != nil {
		return fmt.Errorf("输入密码失败: %v", err)
	}

	// 回读校验4格是否填满
	var filled string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){
		var inputs = document.querySelectorAll('`+selPinContainer+` input');
		var v = '';
		for (var i = 0; i < inputs.length; i++) v += inputs[i].value || '';
		return v;
	})()`, &filled))
	if strings.TrimSpace(filled) == passcode {
		return nil
	}

	// 兜底: 逐格点击输入, 第i位 = 容器内第i个子div中的input
	m.logger.Print("TW_MSG2", "连续输入未自动进格, 改为逐格输入")
	for i, ch := range passcode {
		sel := fmt.Sprintf(`%s div:nth-child(%d) input`, selPinContainer, i+1)
		perCtx, cancelPer := context.WithTimeout(ctx, 8*time.Second)
		err := chromedp.Run(perCtx,
			chromedp.Click(sel, chromedp.ByQuery),
			chromedp.SendKeys(sel, string(ch), chromedp.ByQuery),
		)
		cancelPer()
		if err != nil {
			return fmt.Errorf("逐格输入密码第%d位失败: %v", i+1, err)
		}
	}
	return nil
}

// submitPasscode 若密码界面仍在则回车提交(4位填满后部分场景会自动提交)
func (m *twitterMessenger) submitPasscode(ctx context.Context) error {
	if !m.pinInputVisible(ctx) {
		return nil // 已自动提交
	}
	enterCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := chromedp.Run(enterCtx, chromedp.KeyEvent("\r"))
	cancel()
	if err != nil {
		return fmt.Errorf("回车提交 Passcode 失败: %v", err)
	}
	time.Sleep(2 * time.Second)
	return nil
}

// waitPinGone 等待密码界面消失(跳转回私信主页面)
func (m *twitterMessenger) waitPinGone(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.pinInputVisible(ctx) {
			return nil
		}
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("Passcode 输入后未跳转回私信主页面(密码可能错误或界面异常)")
}

// OpenConversationFromProfile 发起新会话并搜索目标账号:
// New message -> 搜索框输入 account_name -> 点击结果第一项 -> 等待聊天窗口就绪
func (m *twitterMessenger) OpenConversationFromProfile(ctx context.Context, task message.SendTask) error {
	accountName := strings.TrimSpace(task.AccountName)
	if accountName == "" {
		return errors.New("缺少目标账号名 account_name")
	}

	// 点击 New message 按钮
	m.logger.Print("TW_MSG3", "点击 New message 按钮")
	clicked := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !clicked {
		clickCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := chromedp.Run(clickCtx, chromedp.Click(selNewChatBtn, chromedp.ByQuery))
		cancel()
		if err == nil {
			clicked = true
			break
		}
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !clicked {
		return errors.New("30秒内未找到 New message 按钮")
	}

	// 等待联系人搜索框出现
	waitCtx, cancelWait := context.WithTimeout(ctx, 15*time.Second)
	defer cancelWait()
	if err := chromedp.Run(waitCtx, chromedp.WaitVisible(selSearchInput, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("未出现联系人搜索框: %v", err)
	}
	time.Sleep(1 * time.Second)

	// 点击搜索框并输入目标账号名
	m.logger.Print("TW_MSG3", "搜索目标账号: "+accountName)
	typeCtx, cancelType := context.WithTimeout(ctx, 15*time.Second)
	defer cancelType()
	if err := chromedp.Run(typeCtx,
		chromedp.Click(selSearchInput, chromedp.ByQuery),
		chromedp.SendKeys(selSearchInput, accountName, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("输入搜索账号名失败: %v", err)
	}

	// 等待搜索结果并点击第一项: 在唯一搜索结果容器内取第一个建议项(DOM顺序即列表顺序),
	// 不使用页面文字, 也不依赖会被账号ID动态拼接的 data-testid 精确值。
	found := false
	resultDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(resultDeadline) && !found {
		evalCtx, cancelEval := context.WithTimeout(ctx, 5*time.Second)
		err := chromedp.Run(evalCtx, chromedp.Evaluate(`(function(){
			var list = document.querySelector('`+selSuggestList+`');
			if (!list) return false;
			var item = list.querySelector('[data-testid^="new-dm-user-suggestion-"]');
			if (!item) return false;
			item.click();
			return true;
		})()`, &found))
		cancelEval()
		if err == nil && found {
			break
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !found {
		return fmt.Errorf("30秒内未搜索到账号 %s 的结果", accountName)
	}
	m.logger.Print("TW_MSG3", "已点击搜索结果第一项, 进入会话")

	// 等待聊天窗口(消息输入框)就绪
	chatCtx, cancelChat := context.WithTimeout(ctx, 20*time.Second)
	defer cancelChat()
	if err := chromedp.Run(chatCtx, chromedp.WaitVisible(selComposer, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("未进入聊天窗口: %v", err)
	}
	time.Sleep(2 * time.Second)
	return nil
}

// SendInConversation 点击消息输入框填入内容, 回车发送, 并验证消息出现在聊天记录中
func (m *twitterMessenger) SendInConversation(ctx context.Context, content string) error {
	m.logger.Print("TW_MSG4", "填写消息内容")
	typeCtx, cancelType := context.WithTimeout(ctx, 15*time.Second)
	defer cancelType()
	if err := chromedp.Run(typeCtx,
		chromedp.Click(selComposer, chromedp.ByQuery),
		chromedp.SendKeys(selComposer, content, chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("填写消息内容失败: %v", err)
	}
	time.Sleep(1 * time.Second)

	// 回车发送
	m.logger.Print("TW_MSG4", "回车发送消息")
	enterCtx, cancelEnter := context.WithTimeout(ctx, 5*time.Second)
	if err := chromedp.Run(enterCtx, chromedp.KeyEvent("\r")); err != nil {
		cancelEnter()
		return fmt.Errorf("回车发送失败: %v", err)
	}
	cancelEnter()

	// 成功判定: 发送的内容已显示在聊天记录中
	return m.verifySent(ctx, content)
}

// verifySent 轮询聊天记录容器, 确认发送的内容已显示在页面消息列表中
// (按发送内容本身匹配, 内容为接口传入数据而非界面文案, 不受界面语言影响)
func (m *twitterMessenger) verifySent(ctx context.Context, content string) error {
	needle := strings.TrimSpace(content)
	scrollerJS := `document.querySelector('` + selMessageScroller + `') || document.querySelector('` +
		`div[data-testid="dm-message-list-container"]` + `')`
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		evalCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := chromedp.Run(evalCtx, chromedp.Evaluate(fmt.Sprintf(`(function(needle){
			var scroller = %s;
			if (!scroller) return false;
			return (scroller.innerText || '').indexOf(needle) !== -1;
		})(%q)`, scrollerJS, needle), &ok))
		cancel()
		if err == nil && ok {
			m.logger.Print("TW_MSG5", "消息已出现在聊天记录中, 发送成功")
			return nil
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return fmt.Errorf("验证发送结果时上下文超时: %v", ctx.Err())
		}
	}
	return errors.New("20秒内未在聊天记录中发现发送的内容, 判定发送失败")
}

// FetchConversationList 解析收件箱会话列表(先处理可能出现的 Passcode 拦截)。
// conversation_id 优先取会话链接; 无链接时记录收件箱内序号(inbox-idx:N), 供 OpenConversation 结构化定位。
func (m *twitterMessenger) FetchConversationList(ctx context.Context, opts message.FetchOptions) ([]message.Conversation, error) {
	if err := m.handlePasscode(ctx, opts.Passcode); err != nil {
		return nil, err
	}

	// 空收件箱: 页面渲染唯一占位标识, 直接返回空列表而不是等待超时
	var empty bool
	emptyCtx, cancelEmpty := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(emptyCtx, chromedp.Evaluate(`!!document.querySelector(`+selEmptyInbox+`)`, &empty))
	cancelEmpty()
	if empty {
		m.logger.Print("TW_MSG3", "收件箱为空")
		return []message.Conversation{}, nil
	}

	sels, _ := json.Marshal(twConversationItemSelectors)
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
			var name = lines[0] || '';
			var a = it.querySelector('a[href]') || (it.tagName === 'A' ? it : null);
			var href = a ? a.href : '';
			return {
				conversation_id: href || ('inbox-idx:' + idx),
				partner_name: name,
				last_message: lines.length > 2 ? lines[lines.length - 2] || '' : '',
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

// OpenConversation 从收件箱打开指定会话(纯结构化定位, 不按显示名文本匹配):
// 优先按会话链接直接导航; 无链接时按拉取时记录的收件箱序号(inbox-idx:N)点击对应项。
func (m *twitterMessenger) OpenConversation(ctx context.Context, conv message.Conversation) error {
	if strings.HasPrefix(conv.ConversationID, "http") {
		if err := chromedp.Run(ctx, chromedp.Navigate(conv.ConversationID), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return err
		}
	} else {
		var idx int
		if _, err := fmt.Sscanf(conv.ConversationID, "inbox-idx:%d", &idx); err != nil {
			return fmt.Errorf("会话缺少可定位的链接或序号(conversation_id=%s)", conv.ConversationID)
		}
		sels, _ := json.Marshal(twConversationItemSelectors)
		js := fmt.Sprintf(`(function(sels, idx){
			for (var i = 0; i < sels.length; i++) {
				var items;
				try { items = document.querySelectorAll(sels[i]); } catch (e) { continue; }
				if (items.length && idx >= 0 && idx < items.length) {
					items[idx].click();
					return true;
				}
			}
			return false;
		})(%s, %d)`, string(sels), idx)
		var clicked bool
		clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(clickCtx, chromedp.Evaluate(js, &clicked))
		cancel()
		if err != nil {
			return err
		}
		if !clicked {
			return fmt.Errorf("收件箱内未找到第%d个会话项", idx+1)
		}
	}

	// 等待聊天窗口就绪
	chatCtx, cancelChat := context.WithTimeout(ctx, 20*time.Second)
	defer cancelChat()
	_ = chromedp.Run(chatCtx, chromedp.WaitVisible(selComposer, chromedp.ByQuery))
	time.Sleep(2 * time.Second)
	return nil
}

// FetchConversationMessages 解析当前会话的最新消息(时间正序)
// TODO(校准): direction 依据消息项内是否含自己头像判断, 需在有消息的会话中实测校准
func (m *twitterMessenger) FetchConversationMessages(ctx context.Context) ([]message.Message, error) {
	msgSels, _ := json.Marshal(twMessageItemSelectors)
	containerSel := strings.Join(twMessageContainerSelectors, ", ")
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
