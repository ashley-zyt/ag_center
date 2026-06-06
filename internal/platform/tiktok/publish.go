package tiktok

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

// PublishRequest TikTok 发布请求参数
type PublishRequest struct {
	WebsocketURL     string
	Text             string
	VideoPath        string
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

// PublishVideo 连接远程浏览器，打开上传页，上传视频、填写标题，点击发布，
// 然后等待并关闭浏览器，最终删除本地视频文件
func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("TT0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("TT0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("TT0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("TT0 %v", err)
	}

	logger.Print("TT1", "连接浏览器WebSocket")
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, _ := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
		chromedp.WithErrorf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
	)
	defer func() {
		logger.Print("TT7", "关闭标签页")
		_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			closeTabCtx, cancelCloseTab := context.WithTimeout(ctx, 6*time.Second)
			defer cancelCloseTab()
			var result interface{}
			return chromedp.Run(closeTabCtx, chromedp.Evaluate(`window.close()`, &result))
		}))

		logger.Print("TT7", "关闭浏览器窗口")
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancelClose()
		_ = chromedp.Run(closeCtx, browser.Close())

		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancelStop()
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			logger.Print("TT7", "已请求停止Undetectable Profile")
		}
		logger.Print("TT7", "资源清理完成")
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 5*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Run(tabCtx, chromedp.Navigate("https://www.tiktok.com/creator-center/upload"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("TT2 %v", err)
	}
	logger.Print("TT2", "已打开TikTok上传页")

	// 检查是否需要登录
	loginCheckCtx, cancelLoginCheck := context.WithTimeout(tabCtx, 5*time.Second)
	var loginNodes []*cdp.Node
	_ = chromedp.Run(loginCheckCtx, chromedp.Nodes(`//*[contains(text(), 'Log in to TikTok') or contains(text(), 'We regret to inform you that we have discontinued operating TikTok in Hong Kong.')]`, &loginNodes, chromedp.BySearch))
	cancelLoginCheck()
	if len(loginNodes) > 0 {
		return errors.New("TT2 tiktok not logged in in this profile")
	}

	if err := waitAndUploadFile(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("TT3 %v", err)
	}

	_ = dismissPopups(tabCtx, logger)

	if req.Text != "" {
		if err := fillText(tabCtx, logger, req.Text); err != nil {
			return fmt.Errorf("TT5 %v", err)
		}
	}
	logger.Print("TT6", "已填写标题，等待点击发布")
	time.Sleep(30 * time.Second)
	if err := clickPost(tabCtx, logger); err != nil {
		return fmt.Errorf("TT6 %v", err)
	}
	if handleContentCheckLiteModal(tabCtx, logger) {
		if err := clickPost(tabCtx, logger); err != nil {
			return fmt.Errorf("TT6 %v", err)
		}
	}
	logger.Print("TT6", "已点击发布，等待页面跳转")
	expectedURL := "https://www.tiktok.com/tiktokstudio/content"
	redirectDeadline := time.Now().Add(30 * time.Second)
	redirectOK := false
	for time.Now().Before(redirectDeadline) {
		var href string
		locCtx, cancelLoc := context.WithTimeout(tabCtx, 1500*time.Millisecond)
		_ = chromedp.Run(locCtx, chromedp.Location(&href))
		cancelLoc()
		if strings.HasPrefix(href, expectedURL) {
			redirectOK = true
			break
		}
		time.Sleep(800 * time.Millisecond)
	}
	if !redirectOK {
		logger.Print("TT6", "未跳转至TikTok Studio内容页，未知原因需要人为检查")
		time.Sleep(8 * time.Second)
		return errors.New("TT6 未跳转至TikTok Studio内容页，未知原因需要人为检查")
	}
	logger.Print("TT6", "检测到跳转至TikTok Studio内容页，发布成功")
	time.Sleep(8 * time.Second)
	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("TT8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("TT8", "已删除本地视频: "+absVideoPath)
	}
	return nil
}

// handlePostNowModal 检查并处理 “Post now” 确认弹窗
func handlePostNowModal(ctx context.Context, logger *logx.Logger) bool {
	var found bool
	// 检查是否存在指定的按钮，且文本匹配且 aria-disabled 为 false
	js := `(function(){
		var labels = document.querySelectorAll('button[aria-disabled="false"] > div[class="TUXButton-label"]');
		for(var i=0; i<labels.length; i++) {
			if(labels[i].textContent.trim().includes("Post now")) {
				return true;
			}
		}
		return false;
	})()`

	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &found))
	cancel()

	if found {
		logger.Print("TT6", "检测到 'Post now' 确认按钮，进行点击确认")
		// 使用更通用的点击脚本，确保能点到
		jsClick := `(function(){
			var labels = document.querySelectorAll('button[aria-disabled="false"] > div[class="TUXButton-label"]');
			for(var i=0; i<labels.length; i++) {
				if(labels[i].textContent.trim().includes("Post now")) {
					labels[i].parentElement.click();
					return true;
				}
			}
			return false;
		})()`

		var clicked bool
		clickCtx, cancelClick := context.WithTimeout(ctx, 6*time.Second)
		_ = chromedp.Run(clickCtx, chromedp.Evaluate(jsClick, &clicked))
		cancelClick()

		if !clicked {
			logger.Print("TT6", "点击 'Post now' 失败")
			return false
		}

		logger.Print("TT6", "已点击 Post now，等待5秒后重新尝试点击发布")
		time.Sleep(5 * time.Second)
		return true
	}
	return false
}

func handleContentCheckLiteModal(ctx context.Context, logger *logx.Logger) bool {
	var found bool
	js := `(function(){
		var t = document.body ? (document.body.innerText || "") : "";
		t = t.toLowerCase();
		return (t.indexOf("content may be restricted")>=0) || (t.indexOf("content check lite")>=0);
	})()`
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &found))
	cancel()
	if !found {
		return false
	}
	logger.Print("TT6", "检测到内容检查弹窗，尝试关闭")
	closeSel := `div.common-modal-close>div.common-modal-close-icon`
	clickCtx, cancelClick := context.WithTimeout(ctx, 6*time.Second)
	err := chromedp.Run(clickCtx, chromedp.ScrollIntoView(closeSel, chromedp.ByQuery), chromedp.WaitVisible(closeSel, chromedp.ByQuery), chromedp.Click(closeSel, chromedp.ByQuery))
	cancelClick()
	if err == nil {
		logger.Print("TT6", "已关闭内容检查弹窗，等待5秒")
		time.Sleep(5 * time.Second)
		return true
	}
	return false
}

// clickPost 查找并点击“Post”发布按钮，兼容 div/button 变体与可见性状态
func clickPost(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT6", "查找发布按钮")
	type selEntry struct {
		s  string
		by chromedp.QueryOption
	}
	sels := []selEntry{
		{`//div[contains(@class,'button-group')]//div[@data-e2e='post_video_button'][contains(.,'Post')]`, chromedp.BySearch},
		{`div.button-group > div[data-e2e="post_video_button"]`, chromedp.ByQuery},
		{`//div[contains(@class,'button-group')]//button[@data-e2e='post_video_button'][contains(.,'Post')]`, chromedp.BySearch},
		{`div.button-group > button[data-e2e="post_video_button"]`, chromedp.ByQuery},
		{`//button[@data-e2e='post_video_button'][contains(.,'Post')]`, chromedp.BySearch},
		{`//div[contains(@class,'button-group')]//button[@data-e2e='post_video_button' and not(@disabled) and not(@aria-disabled='true')]`, chromedp.BySearch},
		{`//button[@data-e2e='post_video_button' and not(@disabled) and not(@aria-disabled='true')]`, chromedp.BySearch},
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range sels {
			var nodes []*cdp.Node
			stepCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
			_ = chromedp.Run(stepCtx, chromedp.Nodes(e.s, &nodes, e.by))
			cancel()
			if len(nodes) == 0 {
				continue
			}
			logger.Print("TT6", "找到发布按钮: "+e.s)
			clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
			err := chromedp.Run(clickCtx,
				chromedp.ScrollIntoView(e.s, e.by),
				chromedp.WaitVisible(e.s, e.by),
				chromedp.Click(e.s, e.by),
			)
			cancelClick()

			// 点击后检查是否出现 "Post now" 弹窗
			if handlePostNowModal(ctx, logger) {
				// 如果处理了弹窗，等待5秒后（已在handle内等待）继续循环，尝试再次点击主按钮
				continue
			}

			if err == nil {
				return nil
			}
			var ok bool
			js := `(function(sel){
				var el = sel.startsWith("//") ? (document.evaluate(sel, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue) : document.querySelector(sel);
				if(!el) return false;
				try{el.scrollIntoView({block:'center',inline:'center'});}catch(e){}
				try{el.click();return true;}catch(e){return false;}
			})(` + fmt.Sprintf("%q", e.s) + `)`
			eCtx, cancelEval := context.WithTimeout(ctx, 3*time.Second)
			_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &ok))
			cancelEval()
			if ok {
				if handlePostNowModal(ctx, logger) {
					continue
				}
				return nil
			}
		}
		time.Sleep(800 * time.Millisecond)
	}
	return errors.New("TT6 cannot find publish button on tiktok page")
}

// waitAndUploadFile 等待文件上传控件并选择本地视频
func waitAndUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("TT3", "等待视频上传控件")
	uploadSelectors := []string{
		`//input[@type='file']`,
		`//div[@role='dialog']//input[@type='file']`,
		`//input[@data-e2e='upload-video-input']`,
		`//div[@data-e2e='upload-video']//input[@type='file']`,
	}
	var found string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
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
		if found != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if found == "" {
		return errors.New("TT3 video upload control not found within 60 seconds")
	}
	logger.Print("TT3", "使用选择器: "+found)
	if err := chromedp.Run(ctx, chromedp.WaitReady(found, chromedp.BySearch)); err != nil {
		return err
	}
	logger.Print("TT4", "开始选择视频文件: "+absVideoPath)
	return chromedp.Run(ctx, chromedp.SetUploadFiles(found, []string{absVideoPath}, chromedp.BySearch))
}

// dismissPopups 关闭上传过程中的提示弹窗
func dismissPopups(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT9", "尝试关闭提示窗口")
	candidates := []string{
		`//div[@role='dialog']//button[not(@disabled)][contains(.,'Turn on') or contains(.,'Got it') or contains(.,'Continue') or contains(.,'OK') or contains(.,'Next') or contains(.,'Skip')]`,
		`//div[@role='dialog']//div[@role='button' and not(@aria-disabled='true')][contains(.,'Turn on') or contains(.,'Got it') or contains(.,'Continue') or contains(.,'OK') or contains(.,'Next') or contains(.,'Skip')]`,
		`//button[not(@disabled)][contains(.,'Turn on') or contains(.,'Got it') or contains(.,'Continue') or contains(.,'OK') or contains(.,'Next') or contains(.,'Skip')]`,
		`//div[@role='button' and not(@aria-disabled='true')][contains(.,'Turn on') or contains(.,'Got it') or contains(.,'Continue') or contains(.,'OK') or contains(.,'Next') or contains(.,'Skip')]`,
		`//div[@role='dialog']//*[contains(.,'确定') or contains(.,'继续') or contains(.,'知道了') or contains(.,'允许') or contains(.,'打开')]`,
	}
	deadline := time.Now().Add(25 * time.Second)
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
			clickCtx, cancelClick := context.WithTimeout(ctx, 6*time.Second)
			err := chromedp.Run(clickCtx, chromedp.ScrollIntoView(xp, chromedp.BySearch), chromedp.WaitVisible(xp, chromedp.BySearch), chromedp.Click(xp, chromedp.BySearch))
			cancelClick()
			if err == nil {
				logger.Print("TT9", "已关闭提示")
				clicked = true
				break
			}
		}
		if !clicked {
			time.Sleep(600 * time.Millisecond)
		} else {
			time.Sleep(800 * time.Millisecond)
		}
	}
	return nil
}

// fillText 填写标题：对 Draft.js 容器执行全选删除和粘贴，失败则键盘输入兜底
func fillText(ctx context.Context, logger *logx.Logger, text string) error {
	logger.Print("TT5", "填写视频文案")
	// 精确定位到 DraftEditor-editorContainer > public-DraftEditor-content > div[data-contents='true']
	childSel := `div.DraftEditor-editorContainer > div.public-DraftEditor-content > div[data-contents="true"]`
	contentSel := `div.DraftEditor-editorContainer > div.public-DraftEditor-content`
	logger.Print("TT5", "定位标题容器: "+childSel)
	stepCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	err := chromedp.Run(stepCtx,
		chromedp.WaitVisible(childSel, chromedp.ByQuery),
		chromedp.Click(childSel, chromedp.ByQuery),
		chromedp.WaitVisible(contentSel, chromedp.ByQuery),
		chromedp.Focus(contentSel, chromedp.ByQuery),
	)
	cancel()
	if err != nil {
		logger.Print("TT5", "未找到标题元素: "+err.Error())
		return errors.New("TT5 cannot find tiktok caption input")
	}
	logger.Print("TT5", "已点击并获取焦点")
	// 选择全部并删除
	selectCtx, cancelSel := context.WithTimeout(ctx, 4*time.Second)
	err = chromedp.Run(selectCtx,
		chromedp.SendKeys(contentSel, kb.Control+"a", chromedp.ByQuery),
		chromedp.SendKeys(contentSel, kb.Delete, chromedp.ByQuery),
	)
	cancelSel()
	if err != nil {
		logger.Print("TT5", "全选删除失败: "+err.Error())
	} else {
		logger.Print("TT5", "已全选并清空")
	}
	// 粘贴标题（优先使用 insertText，失败则回退为直接文本输入）
	var ok bool
	js := fmt.Sprintf(`(function(T){
		var el=document.querySelector(%q);
		if(!el){return false;}
		el.focus();
		try{
			var sel=window.getSelection(); if(sel){sel.removeAllRanges(); var r=document.createRange(); r.selectNodeContents(el); sel.addRange(r);}
		}catch(e){}
		try{if(document.execCommand('insertText', false, T)) return true;}catch(e){}
		el.textContent=T;
		try{el.dispatchEvent(new InputEvent('input',{bubbles:true}));}catch(e){el.dispatchEvent(new Event('input',{bubbles:true}));}
		return true;
	})(%q)`, contentSel, text)
	typeCtx, cancelType := context.WithTimeout(ctx, 5*time.Second)
	if err := chromedp.Run(typeCtx, chromedp.Evaluate(js, &ok)); err != nil {
		logger.Print("TT5", "插入文本执行异常: "+err.Error())
	}
	cancelType()
	if !ok {
		// 兜底使用键盘输入
		type2Ctx, cancelType2 := context.WithTimeout(ctx, 5*time.Second)
		if err := chromedp.Run(type2Ctx, chromedp.SendKeys(contentSel, text, chromedp.ByQuery)); err != nil {
			cancelType2()
			logger.Print("TT5", "键盘兜底输入失败: "+err.Error())
			return err
		}
		cancelType2()
		logger.Print("TT5", "插入文本失败，已使用键盘兜底")
	} else {
		logger.Print("TT5", "使用insertText粘贴成功")
	}
	logger.Print("TT5", "文本已填写")
	return nil
}

func dumpElementSubtree(ctx context.Context, logger *logx.Logger, selector string, tag string) {
	var dom interface{}
	js := `(function(sel){
		var el=document.querySelector(sel);
		if(!el) return {found:false, reason:'not found', selector:sel};
		function outline(node, depth, out){
			var info={depth:depth};
			if(node.nodeType===1){
				info.tag=(node.tagName||'').toLowerCase();
				var attrs={};
				for(var i=0;i<node.attributes.length;i++){
					var a=node.attributes[i];
					attrs[a.name]=a.value;
				}
				info.attrs=attrs;
				var txt=node.textContent||'';
				info.textLen=txt.length;
				info.textSnippet=txt.length>200?txt.slice(0,200):txt;
				var dt=node.getAttribute('data-text');
				if(dt!==null){info.dataText=dt;}
			}else if(node.nodeType===3){
				info.tag='#text';
				var t=node.nodeValue||'';
				info.textLen=t.length;
				info.textSnippet=t.length>200?t.slice(0,200):t;
			}else{
				info.tag='#node'+node.nodeType;
			}
			out.push(info);
			var children=node.childNodes||[];
			for(var j=0;j<children.length;j++){
				outline(children[j], depth+1, out);
			}
		}
		var res=[];
		outline(el,0,res);
		return {found:true,count:res.length,tree:res};
	})(` + fmt.Sprintf("%q", selector) + `)`
	eCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &dom))
	cancel()
}

func dumpDraftEditorContainers(ctx context.Context, logger *logx.Logger) {
	var dom interface{}
	js := `(function(){
		function outline(el){
			if(!el) return null;
			var res=[];
			function walk(node, depth){
				var info={depth:depth};
				if(node.nodeType===1){
					info.tag=(node.tagName||'').toLowerCase();
					var attrs={};
					for(var i=0;i<node.attributes.length;i++){
						var a=node.attributes[i];
						attrs[a.name]=a.value;
					}
					info.attrs=attrs;
					var txt=node.textContent||'';
					info.textLen=txt.length;
					info.textSnippet=txt.length>160?txt.slice(0,160):txt;
					var dt=node.getAttribute('data-text');
					if(dt!==null){info.dataText=dt;}
				}else if(node.nodeType===3){
					info.tag='#text';
					var t=node.nodeValue||'';
					info.textLen=t.length;
					info.textSnippet=t.length>160?t.slice(0,160):t;
				}else{
					info.tag='#node'+node.nodeType;
				}
				res.push(info);
				var children=node.childNodes||[];
				for(var j=0;j<children.length;j++){
					walk(children[j], depth+1);
				}
			}
			walk(el,0);
			return res;
		}
		var containers=Array.prototype.slice.call(document.querySelectorAll('div.DraftEditor-editorContainer'));
		var contents=Array.prototype.slice.call(document.querySelectorAll('div.public-DraftEditor-content'));
		return {
			container_count: containers.length,
			content_count: contents.length,
			containers: containers.slice(0,2).map(outline),
			contents: contents.slice(0,2).map(outline)
		};
	})()`
	eCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &dom))
	cancel()
}

func dumpElementOuterHTML(ctx context.Context, logger *logx.Logger, selector string, tag string) {
	var html string
	js := `(function(sel){
		var el = sel.startsWith("//") ? (document.evaluate(sel, document, null, XPathResult.FIRST_ORDERED_NODE_TYPE, null).singleNodeValue) : document.querySelector(sel);
		if(!el) return "";
		return el.outerHTML;
	})(` + fmt.Sprintf("%q", selector) + `)`
	eCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &html))
	cancel()
	if strings.TrimSpace(html) == "" {
		logger.Print(tag, "未找到元素或HTML为空")
		return
	}
}

func clickPublish(ctx context.Context, logger *logx.Logger) error {
	logger.Print("TT6", "尝试点击发布按钮")
	enabledCandidates := []string{
		`//div[contains(@class,'button-group')]//button[@data-e2e='post_video_button' and not(@disabled) and not(@aria-disabled='true')]`,
		`//button[@data-e2e='post_video_button' and not(@disabled) and not(@aria-disabled='true')]`,
		`//button[not(@disabled) and (contains(.,'Post') or contains(.,'Publish') or contains(.,'Upload'))]`,
		`//div[@role='button' and not(@aria-disabled='true')][contains(.,'Post') or contains(.,'Publish') or contains(.,'Upload')]`,
		`//button[@data-e2e='upload-btn' and not(@disabled)]`,
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, xp := range enabledCandidates {
			stepCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
			var nodes []*cdp.Node
			_ = chromedp.Run(stepCtx, chromedp.Nodes(xp, &nodes, chromedp.BySearch))
			cancel()
			if len(nodes) == 0 {
				continue
			}
			clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
			err := chromedp.Run(clickCtx, chromedp.ScrollIntoView(xp, chromedp.BySearch), chromedp.WaitVisible(xp, chromedp.BySearch), chromedp.Click(xp, chromedp.BySearch))
			cancelClick()
			if err != nil {
				var ok bool
				js := `(function(){
					var btn=document.querySelector("button[data-e2e='upload-btn']:not([disabled])")||document.querySelector("button:not([disabled])");
					if(btn){btn.click();return true;}
					return false;
				})()`
				eCtx, cancelEval := context.WithTimeout(ctx, 3*time.Second)
				_ = chromedp.Run(eCtx, chromedp.Evaluate(js, &ok))
				cancelEval()
				if !ok {
					continue
				}
			}
			logger.Print("TT6", "已点击发布")
			if waitPublishEffect(ctx, logger) == nil {
				logger.Print("TT6", "发布效果检测通过")
				return nil
			}
			logger.Print("TT6", "发布效果检测失败，重试")
		}
		time.Sleep(1000 * time.Millisecond)
	}
	return errors.New("TT6 cannot find publish button on tiktok page")
}

func waitPublishEffect(ctx context.Context, logger *logx.Logger) error {
	checkDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(checkDeadline) {
		var textLen int
		script := `(function(){
			var el=document.querySelector("textarea")||document.querySelector("div[role='textbox'][contenteditable='true']");
			if(!el){return -1;}
			var t=(el.value || el.innerText || el.textContent || "");
			return t.trim().length;
		})()`
		textCtx, cancelText := context.WithTimeout(ctx, 1500*time.Millisecond)
		_ = chromedp.Run(textCtx, chromedp.Evaluate(script, &textLen))
		cancelText()
		if textLen == 0 {
			return nil
		}
		time.Sleep(600 * time.Millisecond)
	}
	return errors.New("TT6 publish effect not observed")
}
