package youtube

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
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
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
	Title            string
	Description      string
	VideoPath        string
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("YTB0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("YTB0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("YTB0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("YTB0 %v", err)
	}

	logger.Print("YTB1", "连接浏览器WebSocket")
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, cancelTab := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
		chromedp.WithErrorf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
	)
	defer cancelTab()
	defer func() {
		closeCtx, cancelClose := context.WithTimeout(allocCtx, 6*time.Second)
		_ = chromedp.Run(closeCtx, browser.Close())
		cancelClose()
		logger.Print("YTB12", "关闭浏览器窗口完成")
		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			cancelStop()
			logger.Print("YTB13", "已请求停止Undetectable Profile")
		}
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 6*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Run(tabCtx, chromedp.Navigate("https://studio.youtube.com/channel/%s/videos/upload?d=ud&filter%%5B%%5D&sort=%%7B%%22columnType%%22%%3A%%22date%%22%%2C%%22sortOrder%%22%%3A%%22DESCENDING%%22%%7D"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("YTB2 %v", err)
	}
	logger.Print("YTB2", "已打开YouTube Studio")

	// 检查是否需要重新验证/登录
	loginCheckCtx, cancelLoginCheck := context.WithTimeout(tabCtx, 5*time.Second)
	var loginNodes []*cdp.Node
	_ = chromedp.Run(loginCheckCtx, chromedp.Nodes(`//*[contains(text(), "Verify it’s you") or contains(text(), "Choose an account")]`, &loginNodes, chromedp.BySearch))
	cancelLoginCheck()
	if len(loginNodes) > 0 {
		return errors.New("YTB2 youtube not logged in or verification required in this profile")
	}

	if err := waitAndUploadFile(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("YTB3 %v", err)
	}

	// 检查上传后是否触发身份验证
	verifyCtx, cancelVerify := context.WithTimeout(tabCtx, 10*time.Second)
	var verifyNodes []*cdp.Node
	_ = chromedp.Run(verifyCtx, chromedp.Nodes(`//*[contains(text(), "Verify it's you") or contains(text(), "To continue, we need to confirm it’s really you")]`, &verifyNodes, chromedp.BySearch))
	cancelVerify()
	if len(verifyNodes) > 0 {
		return errors.New("YTB3 account verification required after upload")
	}

	_ = dismissPopups(tabCtx, logger)

	logger.Print("YTB4", "上传完成等待10秒")
	time.Sleep(10 * time.Second)
	if strings.TrimSpace(req.Title) != "" {
		logger.Print("YTB5", "开始填写标题")
		if err := fillTitleTextbox(tabCtx, logger, req.Title); err != nil {
			return fmt.Errorf("YTB5 %v", err)
		}
	}
	if strings.TrimSpace(req.Description) != "" {
		logger.Print("YTB5", "开始填写简介")
		if err := fillDescriptionTextbox(tabCtx, logger, req.Description); err != nil {
			return fmt.Errorf("YTB5 %v", err)
		}
	}
	logger.Print("YTB6", "开始选择是否面向儿童")
	if err := clickSelector(tabCtx, `tp-yt-paper-radio-button[name="VIDEO_MADE_FOR_KIDS_NOT_MFK"] > div#radioContainer`, chromedp.ByQuery); err != nil {
		return fmt.Errorf("YTB6 %v", err)
	}
	logger.Print("YTB6", "已选择非儿童选项")
	logger.Print("YTB7", "检测Next按钮是否存在")
	_ = logRightButtonAreaDOM(tabCtx, logger)
	exists, _ := existsSelector(tabCtx, `ytcp-button#next-button`, chromedp.ByQuery)
	if !exists {
		return errors.New("YTB7 next button not found")
	}
	logger.Print("YTB7", "Next按钮存在")
	logger.Print("YTB8", "等待5秒后点击Next")
	time.Sleep(5 * time.Second)
	if err := clickSelector(tabCtx, `ytcp-button#next-button`, chromedp.ByQuery); err != nil {
		return fmt.Errorf("YTB8 %v", err)
	}
	logger.Print("YTB8", "已点击Next")
	// 第二次 Next
	logger.Print("YTB8-2", "检测Next按钮是否存在")
	exists2, _ := existsSelector(tabCtx, `ytcp-button#next-button`, chromedp.ByQuery)
	if !exists2 {
		return errors.New("YTB8-2 next button not found at step 2")
	}
	logger.Print("YTB8-2", "Next按钮存在，等待5秒后点击Next")
	time.Sleep(5 * time.Second)
	if err := clickSelector(tabCtx, `ytcp-button#next-button`, chromedp.ByQuery); err != nil {
		return fmt.Errorf("YTB8-2 %v", err)
	}
	logger.Print("YTB8-2", "第二次Next已点击")
	// 第三次 Next
	logger.Print("YTB8-3", "检测Next按钮是否存在")
	exists3, _ := existsSelector(tabCtx, `ytcp-button#next-button`, chromedp.ByQuery)
	if !exists3 {
		return errors.New("YTB8-3 next button not found at step 3")
	}
	logger.Print("YTB8-3", "Next按钮存在，等待5秒后点击Next")
	time.Sleep(5 * time.Second)
	if err := clickSelector(tabCtx, `ytcp-button#next-button`, chromedp.ByQuery); err != nil {
		return fmt.Errorf("YTB8-3 %v", err)
	}
	logger.Print("YTB8-3", "第三次Next已点击")
	logger.Print("YTB9", "等待5秒后选择可视范围PUBLIC")
	time.Sleep(5 * time.Second)
	if err := clickSelector(tabCtx, `tp-yt-paper-radio-group#privacy-radios > tp-yt-paper-radio-button[name="PUBLIC"]`, chromedp.ByQuery); err != nil {
		return fmt.Errorf("YTB9 %v", err)
	}
	logger.Print("YTB10", "已点击发布，开始检测发布结果")
	logger.Print("YTB10", "确认发布按钮检测")
	existsDone, _ := existsSelector(tabCtx, `ytcp-button#done-button`, chromedp.ByQuery)
	if existsDone {
		logger.Print("YTB10", "点击确认发布")
		if err := clickSelector(tabCtx, `ytcp-button#done-button`, chromedp.ByQuery); err != nil {
			return fmt.Errorf("YTB10 %v", err)
		}
	} else {
		logger.Print("YTB10", "未找到确认发布按钮")
	}
	time.Sleep(8 * time.Second)
	tabCloseCtx, cancelTabClose := context.WithTimeout(tabCtx, 4*time.Second)
	_ = chromedp.Run(tabCloseCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.Close().Do(ctx)
	}))
	cancelTabClose()
	logger.Print("YTB12", "关闭当前标签页完成")
	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("YTB8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("YTB8", "已删除本地视频: "+absVideoPath)
	}
	return nil
}

func waitAndUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("YTB3", "等待视频上传控件")
	uploadSelectors := []string{
		`//input[@type='file']`,
		`//div[@role='dialog']//input[@type='file']`,
		`//ytcp-uploads-dialog//input[@type='file']`,
		`//div[contains(@class,'upload')]//input[@type='file']`,
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
		found = `//input[@type='file']`
	}
	logger.Print("YTB3", "使用选择器: "+found)
	if err := chromedp.Run(ctx, chromedp.WaitReady(found, chromedp.BySearch)); err != nil {
		return err
	}
	logger.Print("YTB4", "开始选择视频文件: "+absVideoPath)
	return chromedp.Run(ctx, chromedp.SetUploadFiles(found, []string{absVideoPath}, chromedp.BySearch))
}

func dismissPopups(ctx context.Context, logger *logx.Logger) error {
	logger.Print("YTB9", "尝试关闭提示窗口")
	candidates := []string{
		`//div[@role='dialog']//button[not(@disabled)][contains(.,'Got it') or contains(.,'OK') or contains(.,'Continue') or contains(.,'Next') or contains(.,'Skip')]`,
		`//div[@role='dialog']//tp-yt-paper-button[not(@disabled)][contains(.,'Got it') or contains(.,'OK') or contains(.,'Continue') or contains(.,'Next') or contains(.,'Skip')]`,
		`//tp-yt-paper-button[not(@disabled)][contains(.,'Got it') or contains(.,'OK') or contains(.,'Continue') or contains(.,'Next') or contains(.,'Skip')]`,
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		clicked := false
		for _, xp := range candidates {
			stepCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			var nodes []*cdp.Node
			_ = chromedp.Run(stepCtx, chromedp.Nodes(xp, &nodes, chromedp.BySearch))
			cancel()
			if len(nodes) == 0 {
				continue
			}
			clickCtx, cancelClick := context.WithTimeout(ctx, 8*time.Second)
			err := chromedp.Run(clickCtx,
				chromedp.ScrollIntoView(xp, chromedp.BySearch),
				chromedp.WaitVisible(xp, chromedp.BySearch),
				chromedp.Click(xp, chromedp.BySearch),
			)
			cancelClick()
			if err == nil {
				logger.Print("YTB9", "已关闭提示")
				clicked = true
				break
			}
		}
		if !clicked {
			time.Sleep(500 * time.Millisecond)
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil
}

func fillTitle(ctx context.Context, logger *logx.Logger, title string) error {
	logger.Print("YTB5", "填写标题")
	selectors := []struct {
		S string
		B chromedp.QueryOption
	}{
		{S: `div.title div#textbox`, B: chromedp.ByQuery},
	}
	for _, s := range selectors {
		stepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(stepCtx,
			chromedp.WaitVisible(s.S, s.B),
			chromedp.Click(s.S, s.B),
			chromedp.Focus(s.S, s.B),
		)
		cancel()
		if err == nil {
			typeCtx, cancelType := context.WithTimeout(ctx, 5*time.Second)
			var ok bool
			js := fmt.Sprintf(`(function(T){
				var el = (%q.startsWith("//")) ?
					(document.evaluate(%q, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue) :
					document.querySelector(%q);
				if(!el) return false;
				try{document.execCommand('selectAll', false, null);}catch(e){}
				try{document.execCommand('insertText', false, '');}catch(e){}
				try{if(document.execCommand('insertText', false, T)) return true;}catch(e){}
				if(el.tagName==='TEXTAREA'){el.value=T;} else {el.textContent=T; el.innerText=T;}
				try{el.dispatchEvent(new InputEvent('input',{bubbles:true}));}catch(e){el.dispatchEvent(new Event('input',{bubbles:true}));}
				return true;
			})(%q)`, s.S, s.S, s.S, title)
			_ = chromedp.Run(typeCtx, chromedp.Evaluate(js, &ok))
			cancelType()
			if ok {
				logger.Print("YTB5", "标题已填写")
				return nil
			}
			type2Ctx, cancelType2 := context.WithTimeout(ctx, 5*time.Second)
			err2 := chromedp.Run(type2Ctx, chromedp.SendKeys(s.S, kb.Control+"a", s.B), chromedp.SendKeys(s.S, kb.Delete, s.B), chromedp.SendKeys(s.S, title, s.B))
			cancelType2()
			if err2 == nil {
				logger.Print("YTB5", "标题已键盘兜底填写")
				return nil
			}
		}
	}
	return errors.New("YTB5 cannot find youtube title input")
}

func fillTitleTextbox(ctx context.Context, logger *logx.Logger, title string) error {
	if title != "" {
		truncated := truncateRunes(title, 100)
		if truncated != title {
			logger.Print("YTB5", "标题超过100字符，已截断")
		}
		title = truncated
	}
	return fillTitle(ctx, logger, title)
}

func fillDescriptionTextbox(ctx context.Context, logger *logx.Logger, description string) error {
	if description == "" {
		return nil
	}
	logger.Print("YTB5", "填写简介")
	sel := `div.description div#textbox`
	stepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := chromedp.Run(stepCtx,
		chromedp.WaitVisible(sel, chromedp.ByQuery),
		chromedp.Click(sel, chromedp.ByQuery),
		chromedp.Focus(sel, chromedp.ByQuery),
	)
	cancel()
	if err == nil {
		typeCtx, cancelType := context.WithTimeout(ctx, 5*time.Second)
		var ok bool
		js := fmt.Sprintf(`(function(T){
			var el = document.querySelector('div.description div#textbox');
			if(!el) return false;
			try{document.execCommand('selectAll', false, null);}catch(e){}
			try{document.execCommand('insertText', false, '');}catch(e){}
			try{if(document.execCommand('insertText', false, T)) return true;}catch(e){}
			if(el.tagName==='TEXTAREA'){el.value=T;} else {el.textContent=T; el.innerText=T;}
			try{el.dispatchEvent(new InputEvent('input',{bubbles:true}));}catch(e){el.dispatchEvent(new Event('input',{bubbles:true}));}
			return true;
		})(%q)`, description)
		_ = chromedp.Run(typeCtx, chromedp.Evaluate(js, &ok))
		cancelType()
		if ok {
			logger.Print("YTB5", "简介已填写")
			return nil
		}
		type2Ctx, cancelType2 := context.WithTimeout(ctx, 5*time.Second)
		err2 := chromedp.Run(type2Ctx, chromedp.SendKeys(sel, kb.Control+"a", chromedp.ByQuery), chromedp.SendKeys(sel, kb.Delete, chromedp.ByQuery), chromedp.SendKeys(sel, description, chromedp.ByQuery))
		cancelType2()
		if err2 == nil {
			logger.Print("YTB5", "简介已键盘兜底填写")
			return nil
		}
	}
	return errors.New("YTB5 cannot find youtube description input")
}

func clickSelector(ctx context.Context, selector string, by chromedp.QueryOption) error {
	clickCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := chromedp.Run(clickCtx,
		chromedp.WaitReady(selector, by),
		chromedp.ScrollIntoView(selector, by),
		chromedp.WaitVisible(selector, by),
		chromedp.Click(selector, by),
	); err != nil {
		var ok bool
		js := fmt.Sprintf(`(function(sel){
			var el = sel.startsWith("//") ? null : document.querySelector(sel);
			if(!el) return false;
			try{el.click();return true;}catch(e){return false;}
		})(%q)`, selector)
		eCtx, cancelEval := context.WithTimeout(ctx, 4*time.Second)
		_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &ok))
		cancelEval()
		if ok {
			return nil
		}
		return err
	}
	return nil
}

func existsSelector(ctx context.Context, selector string, by chromedp.QueryOption) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var nodes []*cdp.Node
	err := chromedp.Run(checkCtx, chromedp.Nodes(selector, &nodes, by))
	return len(nodes) > 0, err
}

func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) > max {
		return string(rs[:max])
	}
	return s
}

func logRightButtonAreaDOM(ctx context.Context, logger *logx.Logger) error {
	var html string
	stepCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = chromedp.Run(stepCtx, chromedp.OuterHTML(`div.right-button-area.style-scope.ytcp-uploads-dialog`, &html, chromedp.ByQuery))
	cancel()
	if strings.TrimSpace(html) == "" {
		stepCtx2, cancel2 := context.WithTimeout(ctx, 4*time.Second)
		_ = chromedp.Run(stepCtx2, chromedp.OuterHTML(`//div[@class='right-button-area style-scope ytcp-uploads-dialog']`, &html, chromedp.BySearch))
		cancel2()
	}
	if strings.TrimSpace(html) == "" {
		logger.Print("YTB7", "right-button-area 未找到")
		return errors.New("YTB7 right-button-area not found")
	}
	logger.Print("YTB7", "right-button-area DOM: "+html)
	return nil
}
