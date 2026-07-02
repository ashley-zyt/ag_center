package twitter

import (
	"context"
	"errors"
	"fmt"
	"minimax_pro/internal/chromedputil"
	"minimax_pro/internal/undetectable"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minimax_pro/internal/logx"

	"github.com/chromedp/cdproto/cdp"
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
	Text             string
	VideoPath        string
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("TW0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("TW0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("TW0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("TW0 %v", err)
	}

	logger.Print("TW1", "连接浏览器WebSocket")

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, _ := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(format string, v ...interface{}) {
			(&filterLogger{logger: logger}).Printf(format, v...)
		}),
		chromedp.WithErrorf(func(format string, v ...interface{}) {
			(&filterLogger{logger: logger}).Printf(format, v...)
		}),
	)
	defer func() {
		logger.Print("TW7", "关闭所有标签页")
		closeCtx, cancelClose := context.WithTimeout(allocCtx, 10*time.Second)
		if err := chromedputil.CloseAllTabsThenBrowser(closeCtx); err != nil {
			logger.Print("TW7", "关闭标签页失败: "+err.Error())
		} else {
			logger.Print("TW7", "已关闭所有标签页")
		}
		cancelClose()
		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			cancelStop()
			logger.Print("TW7", "已请求停止Undetectable Profile")
		}
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 4*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Run(tabCtx, chromedp.Navigate("https://x.com/compose/tweet"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("TW2 %v", err)
	}
	logger.Print("TW2", "已打开推文发布页")

	loginCtx, cancelLogin := context.WithTimeout(tabCtx, 3*time.Second)
	defer cancelLogin()
	var loginInputs []*cdp.Node
	_ = chromedp.Run(loginCtx, chromedp.Nodes(`input[name="text"]`, &loginInputs, chromedp.ByQuery))
	if len(loginInputs) > 0 {
		return errors.New("TW2 twitter not logged in in this profile")
	}

	if err := waitAndUploadFile(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("TW3 %v", err)
	}

	if req.Text != "" {
		if err := fillText(tabCtx, logger, req.Text); err != nil {
			return fmt.Errorf("TW5 %v", err)
		}
	}

	logger.Print("TW6", "等待100秒后点击发布")
	time.Sleep(100 * time.Second)

	if err := clickPublish(tabCtx, logger); err != nil {
		return fmt.Errorf("TW6 %v", err)
	}

	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("TW8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("TW8", "已删除本地视频: "+absVideoPath)
	}
	logger.Print("TW6", "发布流程已触发")
	return nil
}

func waitAndUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("TW3", "等待视频上传控件")
	uploadSelectors := []string{
		`//input[@type="file"]`,
		`//div[@role="dialog"]//input[@type="file"]`,
		`//input[@data-testid='fileInput']`,
		`//div[@data-testid='fileInput']//input[@type='file']`,
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
		found = `//input[@type="file"]`
	}
	logger.Print("TW3", "使用选择器: "+found)
	if err := chromedp.Run(ctx, chromedp.WaitReady(found, chromedp.BySearch)); err != nil {
		return err
	}
	logger.Print("TW4", "开始选择视频文件: "+absVideoPath)
	return chromedp.Run(ctx, chromedp.SetUploadFiles(found, []string{absVideoPath}, chromedp.BySearch))
}

func fillText(ctx context.Context, logger *logx.Logger, text string) error {
	logger.Print("TW5", "填写推文文本")
	r := []rune(text)
	if len(r) > 280 {
		text = string(r[:280])
		logger.Print("TW5", "文本超过280字符，已截断")
	}
	sel := `[data-testid="tweetTextarea_0"]`
	stepCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	err := chromedp.Run(stepCtx, chromedp.WaitVisible(sel, chromedp.ByQuery))
	cancel()
	if err != nil {
		return fmt.Errorf("TW5 tweet textarea not visible: %v", err)
	}

	// 先用 JS 清空内容
	clearJs := `(function(){
		var el=document.querySelector("[data-testid='tweetTextarea_0']");
		if(!el) return false;
		el.focus();
		var s=window.getSelection();
		var r=document.createRange();
		r.selectNodeContents(el);
		r.collapse(false);
		s.removeAllRanges();
		s.addRange(r);
		document.execCommand('selectAll',false,null);
		return true;
	})()`
	_ = chromedp.Run(ctx, chromedp.Evaluate(clearJs, nil))
	time.Sleep(500 * time.Millisecond)

	// 用 SendKeys 逐字输入，触发 DraftJS 原生键盘事件
	typeCtx, cancelType := context.WithTimeout(ctx, 60*time.Second)
	for _, ch := range text {
		c := string(ch)
		if c == " " {
			c = " "
		}
		if err := chromedp.Run(typeCtx, chromedp.SendKeys(sel, c, chromedp.ByQuery)); err != nil {
			cancelType()
			return fmt.Errorf("TW5 send key failed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelType()

	// 验证内容
	var confirmed bool
	confirmJs := fmt.Sprintf(`(function(){
		var el=document.querySelector("[data-testid='tweetTextarea_0']");
		if(!el) return false;
		var content=el.textContent||"";
		var inner=el.querySelector('.public-DraftEditor-content');
		if(inner) content=inner.textContent||"";
		return content.length>0;
	})()`)
	_ = chromedp.Run(ctx, chromedp.Evaluate(confirmJs, &confirmed))
	if confirmed {
		logger.Print("TW5", "文本已填写")
		return nil
	}

	// 兜底：再试一次
	typeCtx2, cancelType2 := context.WithTimeout(ctx, 60*time.Second)
	_ = chromedp.Run(typeCtx2, chromedp.SendKeys(sel, kb.Control+"a", chromedp.ByQuery))
	_ = chromedp.Run(typeCtx2, chromedp.SendKeys(sel, kb.Delete, chromedp.ByQuery))
	for _, ch := range text {
		_ = chromedp.Run(typeCtx2, chromedp.SendKeys(sel, string(ch), chromedp.ByQuery))
		time.Sleep(5 * time.Millisecond)
	}
	cancelType2()

	_ = chromedp.Run(ctx, chromedp.Evaluate(confirmJs, &confirmed))
	if confirmed {
		logger.Print("TW5", "文本已填写（兜底）")
		return nil
	}

	return errors.New("TW5 cannot fill tweet text: content not confirmed")
}

func clickPublish(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TW6", "尝试点击发布按钮")
	_ = logButtonStructure(ctx, logger)
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		js := `(function(){
			var btn = document.querySelector('button[data-testid="tweetButton"]');
			if(!btn) return false;
			try{btn.scrollIntoView({block:"center",inline:"center"});}catch(e){}
			try{btn.click();return true;}catch(e){return false;}
		})()`
		eCtx, cancelEval := context.WithTimeout(ctx, 3*time.Second)
		_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &ok))
		cancelEval()
		if !ok {
			time.Sleep(900 * time.Millisecond)
			continue
		}
		logger.Print("TW6", "已点击发布")
		if err := waitPublishEffect(ctx, logger); err == nil {
			logger.Print("TW6", "发布效果检测通过")
			return nil
		} else if strings.Contains(err.Error(), "media failed to upload") {
			return err
		}
		logger.Print("TW6", "发布效果检测失败，重试")
		time.Sleep(1000 * time.Millisecond)
	}
	return errors.New("TW6 cannot find publish button on twitter page")
}

func logButtonStructure(ctx context.Context, logger *logx.Logger) error {
	var html string
	js := `(function(){
		var el = document.querySelector('div[aria-labelledby="modal-header"] > div[data-viewportview="true"] > button[data-testid="tweetButton"]');
		if(!el) {
			el = document.querySelector('button[data-testid="tweetButton"]');
		}
		if(!el) {
			el = document.querySelector('div[aria-labelledby="modal-header"]');
		}
		return el ? el.outerHTML : "NOT_FOUND";
	})()`
	stepCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	_ = chromedp.Run(stepCtx, chromedp.Evaluate(js, &html))
	cancel()

	if html == "" || html == "NOT_FOUND" {
		logger.Print("TW6", "未找到发布按钮及其容器结构")
		return errors.New("TW6 button structure not found")
	}
	logger.Print("TW6", "发布按钮元素结构: "+html)
	return nil
}

func waitPublishEffect(ctx context.Context, logger *logx.Logger) error {
	checkDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(checkDeadline) {
		var mediaUploadFailed bool
		failScript := `(function(){
			var t=document.body?(document.body.innerText||""):"";
			return t.indexOf("Some of your media failed to upload")>=0;
		})()`
		failCtx, cancelFail := context.WithTimeout(ctx, 1500*time.Millisecond)
		_ = chromedp.Run(failCtx, chromedp.Evaluate(failScript, &mediaUploadFailed))
		cancelFail()
		if mediaUploadFailed {
			logger.Print("TW6", "检测到媒体上传失败提示，判定账号异常")
			return errors.New("TW6 account banned: some of your media failed to upload")
		}

		// 检查可用按钮是否消失或变为禁用
		var btnNodes []*cdp.Node
		btnCtx, cancelBtn := context.WithTimeout(ctx, 1500*time.Millisecond)
		_ = chromedp.Run(btnCtx, chromedp.Nodes(`//button[@data-testid='tweetButton' and not(@disabled) and not(@aria-disabled='true')]`, &btnNodes, chromedp.BySearch))
		cancelBtn()

		// 检查文本框是否清空
		var textLen int
		script := `(function(){
			var el=document.querySelector("[data-testid='tweetTextarea_0']");
			if(!el){el=document.querySelector("div[role='textbox'][contenteditable='true']");}
			if(!el){return -1;}
			var t=(el.innerText||el.textContent||"");
			return t.trim().length;
		})()`
		textCtx, cancelText := context.WithTimeout(ctx, 1500*time.Millisecond)
		_ = chromedp.Run(textCtx, chromedp.Evaluate(script, &textLen))
		cancelText()

		if len(btnNodes) == 0 || textLen == 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("TW6 publish effect not observed")
}
