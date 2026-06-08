package instagram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

type filterLogger struct {
	logger *logx.Logger
}

func (l *filterLogger) Printf(format string, v ...interface{}) {
	msg := ""
	if len(v) > 0 {
		msg = fmt.Sprintf(format, v...)
	} else {
		msg = format
	}
	if strings.Contains(msg, "could not unmarshal event: unknown PrivateNetworkRequestPolicy value") ||
		strings.Contains(msg, "could not unmarshal event: unknown ClientNavigationReason value") {
		return
	}
}

type PublishRequest struct {
	WebsocketURL     string
	Text             string
	VideoPath        string
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("IG0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("IG0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("IG0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("IG0 %v", err)
	}

	logger.Print("IG1", "连接浏览器WebSocket")

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, cancelTab := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(format string, v ...interface{}) {
			(&filterLogger{logger: logger}).Printf(format, v...)
		}),
		chromedp.WithErrorf(func(format string, v ...interface{}) {
			(&filterLogger{logger: logger}).Printf(format, v...)
		}),
	)
	defer cancelTab()

	defer func() {
		logger.Print("IG7", "关闭标签页")
		_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			closeTabCtx, cancelCloseTab := context.WithTimeout(ctx, 6*time.Second)
			defer cancelCloseTab()
			var result interface{}
			return chromedp.Run(closeTabCtx, chromedp.Evaluate(`window.close()`, &result))
		}))

		logger.Print("IG7", "关闭浏览器窗口")
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancelClose()
		_ = chromedp.Run(closeCtx, browser.Close())

		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			cancelStop()
			logger.Print("IG7", "已请求停止Undetectable Profile")
		}
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 5*time.Minute)
	defer cancelTimeout()

	logger.Print("IG2", "打开Instagram首页")
	if err := chromedp.Run(tabCtx, chromedp.Navigate("https://www.instagram.com/"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("IG2 %v", err)
	}
	logger.Print("IG2", "已打开Instagram首页")

	loginCtx, cancelLogin := context.WithTimeout(tabCtx, 5*time.Second)
	var loginNodes []*cdp.Node
	_ = chromedp.Run(loginCtx, chromedp.Nodes(`//*[contains(text(), "Log into Instagram") or contains(text(), "Create new account") or contains(text(), "Log in with Facebook") or contains(text(), "Enter your mobile number")]`, &loginNodes, chromedp.BySearch))
	cancelLogin()
	if len(loginNodes) > 0 {
		return errors.New("IG2 instagram not logged in in this profile")
	}

	time.Sleep(3 * time.Second)

	logger.Print("IG3", "点击创建新帖子按钮")
	if err := clickCreatePost(tabCtx, logger); err != nil {
		return fmt.Errorf("IG3 %v", err)
	}

	if err := waitAndUploadFile(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("IG4 %v", err)
	}

	logger.Print("IG4", "等待Next按钮出现（素材已选择）")
	if err := waitAndClick(tabCtx, logger, `div[role="dialog"]>div[role="button"]`, "Next"); err != nil {
		return fmt.Errorf("IG4 %v", err)
	}

	logger.Print("IG5", "等待Edit页面出现（编辑封面步骤）")
	if err := waitForHeading(tabCtx, logger, "Edit"); err != nil {
		return fmt.Errorf("IG5 %v", err)
	}

	logger.Print("IG5", "点击Next进入下一步")
	if err := waitAndClick(tabCtx, logger, `div[role="dialog"]>div[role="button"]`, "Next"); err != nil {
		return fmt.Errorf("IG5 %v", err)
	}

	logger.Print("IG5", "等待New reel页面出现（输入标题步骤）")
	if err := waitForHeading(tabCtx, logger, "New reel"); err != nil {
		return fmt.Errorf("IG5 %v", err)
	}

	logger.Print("IG5", "查找标题输入框并填写")
	if err := fillReelTitle(tabCtx, logger, req.Text); err != nil {
		return fmt.Errorf("IG5 %v", err)
	}
	time.Sleep(30 * time.Second)

	logger.Print("IG6", "点击Share按钮")
	if err := waitAndClick(tabCtx, logger, `div[role="dialog"]>div[role="button"]`, "Share"); err != nil {
		return fmt.Errorf("IG6 %v", err)
	}

	logger.Print("IG6", "等待Your reel has been shared（发布成功）")
	if err := waitForHeading(tabCtx, logger, "Your reel has been shared"); err != nil {
		return fmt.Errorf("IG6 %v", err)
	}
	logger.Print("IG6", "发布成功")

	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("IG8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("IG8", "已删除本地视频: "+absVideoPath)
	}
	return nil
}

func waitForHeading(ctx context.Context, logger *logx.Logger, text string) error {
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		var found bool
		js := fmt.Sprintf(`(function(){
			var dialog = document.querySelector('div[role="dialog"]');
			if(!dialog) return false;
			if((dialog.textContent||"").includes(%q)) return true;
			return false;
		})()`, text)
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &found))
		cancel()
		if found {
			logger.Print("IG", "找到heading: "+text)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	var dialogHTML string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var d=document.querySelector('div[role="dialog"]');return d?d.outerHTML:'NO_DIALOG';})()`, &dialogHTML))
	logger.Print("IG", "弹窗内容: "+dialogHTML)
	return fmt.Errorf("IG5 heading not found: %s", text)
}

func waitAndClick(ctx context.Context, logger *logx.Logger, parentSel string, buttonText string) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var clicked bool
		js := fmt.Sprintf(`(function(){
			var dialog = document.querySelector('div[role="dialog"]');
			if(!dialog) return false;
			var btns = dialog.querySelectorAll('[role="button"]');
			for(var i=0;i<btns.length;i++){
				var t = (btns[i].textContent||"").trim();
				if(t.includes(%q)){
					try{btns[i].click();return true;}catch(e){return false;}
				}
			}
			return false;
		})()`, buttonText)
		evalCtx, cancelEval := context.WithTimeout(ctx, 6*time.Second)
		_ = chromedp.Run(evalCtx, chromedp.Evaluate(js, &clicked))
		cancelEval()
		if clicked {
			logger.Print("IG", "已点击: "+buttonText)
			time.Sleep(2 * time.Second)
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	var dialogHTML string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`(function(){var d=document.querySelector('div[role="dialog"]');return d?d.outerHTML:'NO_DIALOG';})()`, &dialogHTML))
	logger.Print("IG", "弹窗内容: "+dialogHTML)
	return fmt.Errorf("IG5 cannot find or click button: %s", buttonText)
}

func fillReelTitle(ctx context.Context, logger *logx.Logger, text string) error {
	sel := `div[aria-label="Write a caption..."]`
	logger.Print("IG5", "查找标题输入框: "+sel)

	for retry := 0; retry < 3; retry++ {
		// 1. 等待并确保节点可见
		var nodes []*cdp.Node
		findCtx, cancelFind := context.WithTimeout(ctx, 5*time.Second)
		err := chromedp.Run(findCtx,
			chromedp.WaitVisible(sel, chromedp.ByQuery),
			chromedp.Nodes(sel, &nodes, chromedp.ByQuery),
		)
		cancelFind()

		if err != nil || len(nodes) == 0 {
			logger.Print("IG5", fmt.Sprintf("第 %d 次尝试：未找到输入框，等待后重试...", retry+1))
			time.Sleep(2 * time.Second)
			continue
		}

		// 2. 强行触发物理点击和聚焦（这是激活 Lexical 内部物理 Selection 的关键起点）
		logger.Print("IG5", "触发真实物理点击与强聚焦")
		clickCtx, cancelClick := context.WithTimeout(ctx, 5*time.Second)
		err = chromedp.Run(clickCtx,
			chromedp.Click(sel, chromedp.ByQuery),
			chromedp.Focus(sel, chromedp.ByQuery),
		)
		cancelClick()
		if err != nil {
			logger.Print("IG5", "物理聚焦失败，重试...")
			continue
		}
		time.Sleep(300 * time.Millisecond)

		// 3. 【破局核心】：绕过键盘模拟，使用 insertText 穿透 React 内存状态锁
		logger.Print("IG5", "执行内核级文本插入与状态同步...")

		// 通过全选（selectAll）清理可能存在的残余，然后用 insertText 一口气打穿 Lexical 的 onChange 监听器
		injectJs := fmt.Sprintf(`(function(){
			var el = document.querySelector(%q);
			if(!el) return false;
			
			el.focus();
			
			// 1) 强行全选
			document.execCommand('selectAll', false, null);
			// 2) 调用最底层的受信任文本插入原语，这等同于人工完美的粘贴/键入，
			// 它会强制引发 Lexical 的原生同步，将 text 写入 React 的内存变量中
			var ok = document.execCommand('insertText', false, %q);
			
			// 3) 额外派发原生的合成事件，做双重保险
			var ev = new InputEvent('input', { bubbles: true, cancelable: true });
			el.dispatchEvent(ev);
			
			return ok;
		})()`, sel, text)

		var injectOk bool
		injectCtx, cancelInject := context.WithTimeout(ctx, 8*time.Second)
		_ = chromedp.Run(injectCtx, chromedp.Evaluate(injectJs, &injectOk))
		cancelInject()

		if !injectOk {
			logger.Print("IG5", "内核级注入失败，准备重试流程")
			time.Sleep(1 * time.Second)
			continue
		}

		// 4. 【最终脏状态强制合拢】
		logger.Print("IG5", "注入成功，执行表单最终锁合拢")
		shakeCtx, cancelShake := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(shakeCtx,
			chromedp.SendKeys(sel, " ", chromedp.ByQuery), // 敲空格
			chromedp.Sleep(100*time.Millisecond),
			chromedp.KeyEvent("\u0008"), // 退格删掉
		)
		cancelShake()

		// ==================== 【针对右键线索的全新修复代码】 ====================
		logger.Print("IG5", "模拟右键副作用：强行触发失焦与全局事件刷新")
		blurCtx, cancelBlur := context.WithTimeout(ctx, 5*time.Second)

		// 方案 A: 模拟人类点击发帖窗口的空白头部/背景，或者直接调用 el.blur()
		// 我们通过 JS 强行让输入框失去焦点，并触发 change 事件，逼迫 React 收拢状态
		blurJs := fmt.Sprintf(`(function(){
			var el = document.querySelector(%q);
			if(el) {
				el.blur(); // 强行失焦，这会触发表单的 OnBlur 提交状态
				
				// 模拟派发一个受信任的 change 事件
				var changeEvent = new Event('change', { bubbles: true });
				el.dispatchEvent(changeEvent);
			}
			// 顺便让整个 body 重新计算布局，模拟右键带来的 Reflow
			document.body.offsetHeight; 
		})()`, sel)

		_ = chromedp.Run(blurCtx,
			chromedp.Evaluate(blurJs, nil),
			// 方案 B: 物理点击一下页面的其他无关安全区域（例如发帖弹窗的标题栏，防止点错按钮）
			// 请根据你页面实际情况，点一个空白的 div 标签，比如 `div[role="dialog"]` 的头部
			chromedp.Click(`h1, div[role="presentation"]`, chromedp.ByQuery),
		)
		cancelBlur()
		// =====================================================================

		// 给外部指纹浏览器和 IG 前端 3 秒钟完成异步防抖更新状态
		time.Sleep(3 * time.Second)

		// 5. 最终验证检查
		var currentText string
		checkJs := fmt.Sprintf(`(function(){
			var el = document.querySelector(%q);
			return el ? (el.textContent || '').trim() : '';
		})()`, sel)

		checkCtx, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(checkJs, &currentText))
		cancelCheck()

		if len(currentText) > 0 {
			logger.Print("IG5", "标题已成功打穿 React 状态锁: "+currentText)
			return nil
		}

		logger.Print("IG5", "状态校验未通过，准备重试...")
		time.Sleep(1 * time.Second)
	}

	return errors.New("IG5 内核注入文本流失败")
}

func clickCreatePost(ctx context.Context, logger *logx.Logger) error {
	logger.Print("IG3", "点击 New post 入口")
	sel := `svg[aria-label="New post"]`
	clickCtx, cancelClick := context.WithTimeout(ctx, 8*time.Second)
	err := chromedp.Run(clickCtx,
		chromedp.ScrollIntoView(sel, chromedp.ByQuery),
		chromedp.WaitVisible(sel, chromedp.ByQuery),
		chromedp.Click(sel, chromedp.ByQuery),
	)
	cancelClick()
	if err != nil {
		logger.Print("IG3", "chromedp点击失败，尝试JS: "+err.Error())
		var ok bool
		js := `(function(){
			var svg = document.querySelector('svg[aria-label="New post"]');
			if(!svg) return false;
			var btn = svg.closest('button') || svg.closest('a');
			if(!btn) {try{svg.click();return true;}catch(e){return false;}}
			try{btn.scrollIntoView({block:"center"});}catch(e){}
			try{btn.click();return true;}catch(e){return false;}
		})()`
		evalCtx, cancelEval := context.WithTimeout(ctx, 6*time.Second)
		_ = chromedp.Run(evalCtx, chromedp.Evaluate(js, &ok))
		cancelEval()
		if !ok {
			return errors.New("IG3 cannot click New post entry on instagram")
		}
	}

	time.Sleep(2 * time.Second)

	logger.Print("IG3", "检查是否出现Post标签")
	var postTagFound bool
	postJs := `(function(){
		var els = document.querySelectorAll('svg[aria-label="Post"]');
		for(var i=0;i<els.length;i++){
			var title = els[i].closest('button') || els[i].parentElement;
			if(title && (title.textContent||"").trim().includes("Post")) return true;
		}
		return false;
	})()`
	checkPostCtx, cancelPost := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(checkPostCtx, chromedp.Evaluate(postJs, &postTagFound))
	cancelPost()
	if postTagFound {
		logger.Print("IG3", "检测到Post标签，点击它")
		var postClicked bool
		postClickJs := `(function(){
			var els = document.querySelectorAll('svg[aria-label="Post"]');
			for(var i=0;i<els.length;i++){
				var btn = els[i].closest('button') || els[i].parentElement;
				if(btn && (btn.textContent||"").trim().includes("Post")){
					try{btn.click();return true;}catch(e){return false;}
				}
			}
			return false;
		})()`
		postClickCtx, cancelPostClick := context.WithTimeout(ctx, 6*time.Second)
		_ = chromedp.Run(postClickCtx, chromedp.Evaluate(postClickJs, &postClicked))
		cancelPostClick()
		if !postClicked {
			return errors.New("IG3 cannot click Post tag after New post")
		}
		time.Sleep(2 * time.Second)
	}

	logger.Print("IG3", "等待创建帖子弹窗出现")
	dialogSel := `div[aria-label="Create new post"]`
	waitCtx, cancelWait := context.WithTimeout(ctx, 8*time.Second)
	err = chromedp.Run(waitCtx, chromedp.WaitVisible(dialogSel, chromedp.ByQuery))
	cancelWait()
	if err != nil {
		return fmt.Errorf("IG3 dialog not appeared: %v", err)
	}
	logger.Print("IG3", "创建帖子弹窗已出现")
	time.Sleep(2 * time.Second)
	return nil
}

func waitAndUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("IG4", "等待文件上传控件")
	uploadSelectors := []string{
		`div[aria-label="Create new post"] input[type="file"]`,
		`div[aria-label="Create new post"] input[type="file"][accept*="video"]`,
		`//input[@type="file"][@accept*="video"]`,
		`//input[@type="file"]`,
	}
	var found string
	for _, sel := range uploadSelectors {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var nodes []*cdp.Node
		_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
		cancel()
		if len(nodes) > 0 {
			found = sel
			break
		}
	}
	if found == "" {
		found = `div[aria-label="Create new post"] input[type="file"]`
	}
	logger.Print("IG4", "使用选择器: "+found)
	if err := chromedp.Run(ctx, chromedp.WaitReady(found, chromedp.BySearch)); err != nil {
		return err
	}
	logger.Print("IG4", "开始选择视频文件: "+absVideoPath)
	return chromedp.Run(ctx, chromedp.SetUploadFiles(found, []string{absVideoPath}, chromedp.BySearch))
}

func fillCaption(ctx context.Context, logger *logx.Logger, text string) error {
	logger.Print("IG5", "填写图片/视频描述")
	captionSelectors := []string{
		`//div[@role="textbox"][@aria-label="Write a caption..."]`,
		`//div[@aria-label="Write a caption..."]`,
		`//textarea[@aria-label="Write a caption..."]`,
		`//div[@contenteditable="true"][@aria-label="Write a caption..."]`,
		`//div[@role="dialog"]//textarea`,
	}
	for _, sel := range captionSelectors {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var nodes []*cdp.Node
		_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
		cancel()
		if len(nodes) > 0 {
			logger.Print("IG5", "找到描述输入框: "+sel)
			clickCtx, cancelClick := context.WithTimeout(ctx, 8*time.Second)
			err := chromedp.Run(clickCtx,
				chromedp.ScrollIntoView(sel, chromedp.BySearch),
				chromedp.Click(sel, chromedp.BySearch),
			)
			cancelClick()
			if err == nil {
				var ok bool
				js := fmt.Sprintf(`(function(){
					var el = document.querySelector('%s');
					if(!el) return false;
					try{
						el.focus();
						el.textContent = '';
						el.innerText = '';
						var nativeInputValueSetter = Object.getOwnPropertyDescriptor(window.HTMLDivElement.prototype, 'textContent').set;
						nativeInputValueSetter.call(el, %q);
						el.dispatchEvent(new Event('input', {bubbles: true}));
						return true;
					}catch(e){
						try{
							document.execCommand('selectAll', false, null);
							document.execCommand('insertText', false, %q);
							return true;
						}catch(e2){return false;}
					}
				})()`, sel, text, text)
				typeCtx, cancelType := context.WithTimeout(ctx, 8*time.Second)
				_ = chromedp.Run(typeCtx, chromedp.Evaluate(js, &ok))
				cancelType()
				if ok {
					logger.Print("IG5", "描述已填写")
					return nil
				}
			}
		}
	}
	return errors.New("IG5 cannot find caption input on instagram")
}

func clickShare(ctx context.Context, logger *logx.Logger) error {
	logger.Print("IG6", "查找发布按钮")
	shareSelectors := []string{
		`//button[@type="button"][contains(text(), "Share")]`,
		`//button[@type="button"][contains(text(), "Post")]`,
		`//button[@type="button"][.//span[contains(text(), "Share")]]`,
		`//button[@type="button"][.//span[contains(text(), "Post")]]`,
		`//div[@role="dialog"]//button[@type="button"][contains(text(), "Share")]`,
		`//div[@role="dialog"]//button[@type="button"][contains(text(), "Post")]`,
	}
	for _, sel := range shareSelectors {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var nodes []*cdp.Node
		_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
		cancel()
		if len(nodes) > 0 {
			logger.Print("IG6", "找到发布按钮: "+sel)
			clickCtx, cancelClick := context.WithTimeout(ctx, 8*time.Second)
			err := chromedp.Run(clickCtx,
				chromedp.ScrollIntoView(sel, chromedp.BySearch),
				chromedp.Click(sel, chromedp.BySearch),
			)
			cancelClick()
			if err == nil {
				logger.Print("IG6", "已点击发布")
				return nil
			}
		}
	}

	var ok bool
	js := `(function(){
		var btns = document.querySelectorAll('button[type="button"]');
		for(var i=0;i<btns.length;i++){
			var t = (btns[i].textContent||"").trim();
			if(t==='Share'||t==='Post'){
				try{btns[i].scrollIntoView({block:"center"});}catch(e){}
				try{btns[i].click();return true;}catch(e){return false;}
			}
		}
		return false;
	})()`
	evalCtx, cancelEval := context.WithTimeout(ctx, 6*time.Second)
	_ = chromedp.Run(evalCtx, chromedp.Evaluate(js, &ok))
	cancelEval()
	if ok {
		logger.Print("IG6", "JS点击发布成功")
		return nil
	}
	return errors.New("IG6 cannot find share button on instagram")
}
