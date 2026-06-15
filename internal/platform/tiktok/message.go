package tiktok

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

// SendMessageRequest TikTok 私信请求参数
type SendMessageRequest struct {
	SessionID        string
	TargetPlatform   string
	TargetProfileURL string
	AccountID        string
	BrowserName      string
	MessageContent   string
	WebsocketURL     string
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

// SendMessage 向指定用户发送私信
// 参数说明:
//   SessionID: 会话ID
//   TargetPlatform: 目标平台
//   TargetProfileURL: 目标用户主页链接
//   AccountID: 使用的账号ID
//   BrowserName: 使用的指纹浏览器名称
//   MessageContent: 消息内容
func SendMessage(ctx context.Context, logger *logx.Logger, req SendMessageRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("TT_MSG0 websocket_url is required")
	}
	if req.TargetProfileURL == "" {
		return errors.New("TT_MSG0 target_profile_url is required")
	}
	if req.MessageContent == "" {
		return errors.New("TT_MSG0 message_content is required")
	}

	logger.Print("TT_MSG1", fmt.Sprintf("开始私信流程 - 会话ID: %s, 目标平台: %s, 目标主页: %s, 账号ID: %s, 浏览器: %s",
		req.SessionID, req.TargetPlatform, req.TargetProfileURL, req.AccountID, req.BrowserName))

	logger.Print("TT_MSG1", "连接浏览器WebSocket")
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, _ := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
		chromedp.WithErrorf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
	)
	defer func() {
		logger.Print("TT_MSG7", "关闭标签页")
		_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			closeTabCtx, cancelCloseTab := context.WithTimeout(ctx, 6*time.Second)
			defer cancelCloseTab()
			var result interface{}
			return chromedp.Run(closeTabCtx, chromedp.Evaluate(`window.close()`, &result))
		}))

		logger.Print("TT_MSG7", "关闭浏览器窗口")
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancelClose()
		_ = chromedp.Run(closeCtx, browser.Close())

		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancelStop()
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			logger.Print("TT_MSG7", "已请求停止Undetectable Profile")
		}
		logger.Print("TT_MSG7", "资源清理完成")
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 5*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Run(tabCtx, chromedp.Navigate(req.TargetProfileURL), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("TT_MSG2 %v", err)
	}
	logger.Print("TT_MSG2", "已打开目标用户主页: "+req.TargetProfileURL)

	time.Sleep(3 * time.Second)

	// 检查是否需要登录
	loginCheckCtx, cancelLoginCheck := context.WithTimeout(tabCtx, 5*time.Second)
	var loginNodes []*cdp.Node
	_ = chromedp.Run(loginCheckCtx, chromedp.Nodes(`//*[contains(text(), 'Log in') or contains(text(), '登录')]`, &loginNodes, chromedp.BySearch))
	cancelLoginCheck()
	if len(loginNodes) > 0 {
		return errors.New("TT_MSG2 tiktok not logged in in this profile")
	}

	if err := clickMessageButton(tabCtx, logger); err != nil {
		return fmt.Errorf("TT_MSG3 %v", err)
	}

	if err := waitAndFillMessage(tabCtx, logger, req.MessageContent); err != nil {
		return fmt.Errorf("TT_MSG4 %v", err)
	}

	if err := clickSendButton(tabCtx, logger); err != nil {
		return fmt.Errorf("TT_MSG5 %v", err)
	}

	logger.Print("TT_MSG6", "私信发送成功")
	return nil
}

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
	return errors.New("TT_MSG3 cannot find message button on profile page")
}

// waitAndFillMessage 等待消息输入框并填写消息内容
func waitAndFillMessage(ctx context.Context, logger *logx.Logger, message string) error {
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
		return errors.New("TT_MSG4 message input not found within 60 seconds")
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
	})(%q)`, foundSelector, strings.TrimPrefix(foundSelector, "//"), message)

	evalCtx, cancelEval := context.WithTimeout(ctx, 10*time.Second)
	if err := chromedp.Run(evalCtx, chromedp.Evaluate(js, &ok)); err != nil {
		cancelEval()
		return fmt.Errorf("TT_MSG4 evaluate failed: %v", err)
	}
	cancelEval()

	if !ok {
		// 兜底使用SendKeys
		sendCtx, cancelSend := context.WithTimeout(ctx, 10*time.Second)
		if strings.HasPrefix(foundSelector, "//") {
			err := chromedp.Run(sendCtx, chromedp.SendKeys(foundSelector, message, chromedp.BySearch))
			cancelSend()
			if err != nil {
				return fmt.Errorf("TT_MSG4 send keys failed: %v", err)
			}
		} else {
			err := chromedp.Run(sendCtx, chromedp.SendKeys(foundSelector, message, chromedp.ByQuery))
			cancelSend()
			if err != nil {
				return fmt.Errorf("TT_MSG4 send keys failed: %v", err)
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
	return errors.New("TT_MSG5 cannot find send button")
}