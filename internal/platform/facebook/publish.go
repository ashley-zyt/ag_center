package facebook

import (
	"context"
	"errors"
	"fmt"
	"log"
	"minimax_pro/internal/undetectable"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minimax_pro/internal/logx"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

// filterLogger 用于过滤 chromedp 中已知的无害错误日志
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

	// 过滤已知且无害的 unmarshal 错误
	if strings.Contains(msg, "could not unmarshal event: unknown PrivateNetworkRequestPolicy value") ||
		strings.Contains(msg, "could not unmarshal event: unknown ClientNavigationReason value") {
		return
	}

	// 其他日志正常输出，但在前面加上 [CDP] 标识以便区分
	// 注意：chromedp 的日志通常比较底层，如果不是为了调试，也可以选择完全屏蔽
	log.Printf("[CDP] %s", msg)
}

type PublishRequest struct {
	WebsocketURL     string
	Title            string
	VideoPath        string
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("FB0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("FB0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("FB0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("FB0 %v", err)
	}

	logger.Print("FB1", "连接浏览器WebSocket")

	// RemoteAllocator 不需要 DefaultExecAllocatorOptions (那是用于启动新浏览器的)
	// NoModifyURL 是 RemoteAllocatorOption
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	// WithLogf 和 WithErrorf 是 ContextOption，应该传给 NewContext
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
		logger.Print("FB7", "关闭浏览器窗口")
		closeCtx, cancelClose := context.WithTimeout(allocCtx, 5*time.Second)
		defer cancelClose()
		if err := chromedp.Run(closeCtx, browser.Close()); err != nil {
			logger.Print("FB7", "关闭浏览器失败: "+err.Error())
		} else {
			logger.Print("FB7", "已关闭浏览器窗口")
		}
		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			cancelStop()
			logger.Print("FB7", "已请求停止Undetectable Profile")
		}
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 4*time.Minute)
	defer cancelTimeout()

	if err := chromedp.Run(tabCtx, chromedp.Navigate("https://www.facebook.com/"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("FB2 %v", err)
	}
	logger.Print("FB2", "已打开Facebook首页")

	loginCtx, cancelLogin := context.WithTimeout(tabCtx, 5*time.Second)
	defer cancelLogin()
	var loginInputs []*cdp.Node
	_ = chromedp.Run(loginCtx, chromedp.Nodes(`input[name="email"]`, &loginInputs, chromedp.ByQuery))
	if len(loginInputs) > 0 {
		return errors.New("FB2 facebook not logged in in this profile")
	}

	// 检查是否出现人机验证/账号封禁提示
	verifyCtx, cancelVerify := context.WithTimeout(tabCtx, 5*time.Second)
	var verifyNodes []*cdp.Node
	_ = chromedp.Run(verifyCtx, chromedp.Nodes(`//*[contains(translate(text(), 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz'), "confirm you're human to use your account") or contains(translate(text(), 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz'), "we suspect automated behavior on your account") or contains(translate(text(), 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz'), "confirm your identity")]`, &verifyNodes, chromedp.BySearch))
	cancelVerify()
	if len(verifyNodes) > 0 {
		return errors.New("FB2 account verification required")
	}

	if err := waitAndUploadFile(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("FB4 %v", err)
	}

	if req.Title != "" {
		if err := tryFillTitle(tabCtx, logger, req.Title); err != nil {
			return fmt.Errorf("FB5 %v", err)
		}
	}

	if err := setAudiencePublic(tabCtx, logger); err != nil {
		return fmt.Errorf("FB5 %v", err)
	}

	if err := tryPublish(tabCtx, logger); err != nil {
		return fmt.Errorf("FB6 %v", err)
	}

	logger.Print("FB6", "发布流程已触发")

	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("FB8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("FB8", "已删除本地视频: "+absVideoPath)
	}
	return nil
}

func waitAndUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("FB4", "等待视频上传控件")

	// Facebook Reels 页面的上传控件可能不是简单的 input[type="file"]
	// 有时候它是一个隐藏的 input，或者需要先点击某个按钮才会触发
	// 我们尝试多种选择器
	uploadSelectors := []string{
		// 优先尝试弹窗内的上传控件（针对首页发帖弹窗）
		`//div[@role="dialog"]//input[@type="file"]`,
		// 通用选择器
		`input[type="file"]`,
		`//div[contains(@aria-label, "Upload")]//input[@type="file"]`,
		`//div[contains(text(), "Upload")]//input[@type="file"]`,
	}

	var foundSelector string
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range uploadSelectors {
			// 使用较短的超时快速检测
			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			var nodes []*cdp.Node
			if strings.HasPrefix(sel, "//") {
				_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
			} else {
				_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery))
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
		time.Sleep(2 * time.Second)
	}

	if foundSelector == "" {
		return errors.New("FB4 video upload control not found within 100 seconds")
	}

	logger.Print("FB4", "使用选择器: "+foundSelector)

	// 注意：Facebook 的 input[type=file] 往往是 hidden 的，WaitVisible 可能会失败
	// 所以我们改用 WaitReady (只要存在于 DOM 中即可)
	if err := chromedp.Run(ctx, chromedp.WaitReady(foundSelector, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("FB4 %v", err)
	}

	logger.Print("FB4", "开始选择视频文件: "+absVideoPath)
	// SetUploadFiles 不需要元素可见，只要存在即可
	return chromedp.Run(ctx, chromedp.SetUploadFiles(foundSelector, []string{absVideoPath}, chromedp.ByQuery))
}

func tryFillTitle(ctx context.Context, logger *logx.Logger, title string) error {
	logger.Print("FB5", "尝试填写标题")

	selectors := []struct {
		Sel string
		By  chromedp.QueryOption
	}{
		{Sel: `//div[@role="dialog"]//div[@role="textbox"][contenteditable="true"]`, By: chromedp.BySearch},
		{Sel: `div[role="textbox"][contenteditable="true"]`, By: chromedp.ByQuery},
		{Sel: `div[role="textbox"]`, By: chromedp.ByQuery},
		{Sel: `textarea`, By: chromedp.ByQuery},
	}

	for _, s := range selectors {
		stepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(stepCtx,
			chromedp.WaitVisible(s.Sel, s.By),
			chromedp.Focus(s.Sel, s.By),
			chromedp.SendKeys(s.Sel, title, s.By),
		)
		cancel()
		if err == nil {
			logger.Print("FB5", "标题已填写")
			return nil
		}
	}

	return errors.New("FB5 cannot find title input on facebook page")
}

func setAudiencePublic(ctx context.Context, logger *logx.Logger) error {
	logger.Print("FB5", "设置可见范围为Public")

	// 检查当前是否已经是 Public 或 公开
	checkPublicXP := `//div[@role="dialog"]//div[@role="button"][(contains(., "Public") or contains(., "公开") or contains(., "Public View") or contains(., "所有人可见")) and not(contains(@aria-label, "Close"))]`
	var publicNodes []*cdp.Node
	checkCtx, cancelCheck := context.WithTimeout(ctx, 4*time.Second)
	_ = chromedp.Run(checkCtx, chromedp.Nodes(checkPublicXP, &publicNodes, chromedp.BySearch))
	cancelCheck()

	if len(publicNodes) > 0 {
		logger.Print("FB5", "检测到当前可见范围已是 Public/公开，略过设置")
		return nil
	}

	openSelectors := []string{
		`//div[@role="dialog"]//div[@role="button"][contains(., "Friends")]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "朋友")]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "好友")]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Audience")]`,
		`//div[@role="dialog"]//span[contains(text(), "Friends")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//span[contains(text(), "朋友")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//span[contains(text(), "好友")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//span[contains(text(), "الأصدقاء")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Amigos")]`,
		`//div[@role="dialog"]//span[contains(text(), "Amigos")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Amis")]`,
		`//div[@role="dialog"]//span[contains(text(), "Amis")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Freunde")]`,
		`//div[@role="dialog"]//span[contains(text(), "Freunde")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Друзья")]`,
		`//div[@role="dialog"]//span[contains(text(), "Друзья")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "أصدقاء")]`,
		`//div[@role="dialog"]//span[contains(text(), "أصدقاء")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Znajomi")]`,
		`//div[@role="dialog"]//span[contains(text(), "Znajomi")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Arkadaşlar")]`,
		`//div[@role="dialog"]//span[contains(text(), "Arkadaşlar")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Teman")]`,
		`//div[@role="dialog"]//span[contains(text(), "Teman")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "Bạn bè")]`,
		`//div[@role="dialog"]//span[contains(text(), "Bạn bè")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "เพื่อน")]`,
		`//div[@role="dialog"]//span[contains(text(), "เพื่อน")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "友達")]`,
		`//div[@role="dialog"]//span[contains(text(), "友達")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//div[@role="button"][contains(., "친구")]`,
		`//div[@role="dialog"]//span[contains(text(), "친구")]/ancestor::div[@role="button"][1]`,
	}
	_, _ = clickFirstFound(ctx, openSelectors)
	time.Sleep(3 * time.Second)
	menuCtx, cancelMenu := context.WithTimeout(ctx, 6*time.Second)
	_ = chromedp.Run(menuCtx, chromedp.WaitReady(`//div[@role="dialog"]//div[@role="radiogroup"]`, chromedp.BySearch))
	cancelMenu()
	publicLabelCandidates := []string{
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[normalize-space()='Public'] or .//div[contains(.,'Public')] or contains(.,'Public')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'公开')] or contains(.,'公开')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'العامة')] or contains(.,'العامة')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Público')] or contains(.,'Público')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Öffentlich')] or contains(.,'Öffentlich')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Публично')] or contains(.,'Публично')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Herkese Açık')] or contains(.,'Herkese Açık')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Publik')] or contains(.,'Publik')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Umum')] or contains(.,'Umum')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'Công khai')] or contains(.,'Công khai')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'公開')] or contains(.,'公開')]`,
		`//div[@role="radiogroup"]//label[contains(@class,'html-label')][.//span[contains(.,'공개')] or contains(.,'공개')]`,
	}
	clicked, err := clickFirstFound(ctx, publicLabelCandidates)
	if err != nil {
		return err
	}
	if !clicked {
		logger.Print("FB5", "未找到Public，选择第一项")
		// 未匹配到公开，选择 radiogroup 中的第一个单选
		firstXPath := `//div[@role="radiogroup"]//label[contains(@class,'html-label')][1]`
		clickCtx, cancelClick := context.WithTimeout(ctx, 6*time.Second)
		if err := chromedp.Run(clickCtx, chromedp.Click(firstXPath, chromedp.BySearch)); err != nil {
			cancelClick()
			return errors.New("FB5 cannot set audience to Public")
		}
		cancelClick()
	} else {
		logger.Print("FB5", "选择Public")
	}
	// 优先点击“完成”，再回退其他文案
	var confirmSpans []*cdp.Node
	confirmXPath := `//div[@role="dialog"]//form[@method='POST']//div[@role='button']//span[contains(., '完成') or contains(., 'Done') or contains(., 'Save') or contains(., '确定') or contains(., 'OK') or contains(., 'تم') or contains(., 'إنهاء') or contains(., 'Guardar') or contains(., 'Listo') or contains(., 'Salvar') or contains(., 'Concluído') or contains(., 'Terminer') or contains(., 'Valider') or contains(., 'Enregistrer') or contains(., 'Fertig') or contains(., 'Speichern') or contains(., 'Tamam') or contains(., 'Kaydet') or contains(., 'Selesai') or contains(., 'Simpan') or contains(., 'Xong') or contains(., 'Lưu') or contains(., 'เสร็จสิ้น') or contains(., 'บันทึก') or contains(., '完了') or contains(., '保存') or contains(., '완료') or contains(., '저장') or contains(., 'Gotowe') or contains(., 'Zapisz') or contains(., 'Готово') or contains(., 'Сохранить')]`
	countCtx, cancelCount := context.WithTimeout(ctx, 3*time.Second)
	_ = chromedp.Run(countCtx, chromedp.Nodes(confirmXPath, &confirmSpans, chromedp.BySearch))
	cancelCount()
	logger.Print("FB5", fmt.Sprintf("完成按钮候选数量=%d", len(confirmSpans)))
	if len(confirmSpans) > 0 {
		firstBtnXPath := `(` + confirmXPath + `)[1]/ancestor::div[@role='button'][1]`
		clickCtx, cancelClick := context.WithTimeout(ctx, 6*time.Second)
		_ = chromedp.Run(clickCtx, chromedp.Click(firstBtnXPath, chromedp.BySearch))
		cancelClick()
	} else {
		_, _ = clickFirstFound(ctx, []string{
			`//div[@role="dialog"]//div[@role="button"][contains(., "完成")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "保存更改")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "确定")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "保存")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Done")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Save")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "OK")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "تم")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "إنهاء")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Guardar")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Listo")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Salvar")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Concluído")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Terminer")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Valider")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Enregistrer")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Fertig")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Speichern")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Tamam")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Kaydet")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Selesai")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Simpan")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Xong")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Lưu")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "เสร็จสิ้น")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "บันทึก")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "完了")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "保存")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "완료")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "저장")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Gotowe")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Zapisz")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Готово")]`,
			`//div[@role="dialog"]//div[@role="button"][contains(., "Сохранить")]`,
		})
	}
	logger.Print("FB5", "已点击完成")
	// 等待菜单关闭
	waitCloseCtx, cancelClose := context.WithTimeout(ctx, 5*time.Second)
	var menuNodes []*cdp.Node
	for i := 0; i < 5; i++ {
		menuNodes = nil
		_ = chromedp.Run(waitCloseCtx, chromedp.Nodes(`//div[@role="dialog"]//div[@role="radiogroup"]`, &menuNodes, chromedp.BySearch))
		if len(menuNodes) == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	cancelClose()
	return nil
}

func tryPublish(ctx context.Context, logger *logx.Logger) error {
	logger.Print("FB6", "尝试点击发布按钮(可能会经过Next步骤)")

	// 1. 优先尝试首页弹窗的 "Post" 按钮
	// 视频上传可能需要时间处理，按钮可能一开始是 disabled 的
	// 我们轮询等待按钮变为可用 (没有 aria-disabled="true")
	dialogPostSelectors := []string{
		`//div[@role="dialog"]//div[@role="button" and (contains(., "Post") or contains(., "发布"))]`,
		`//div[@role="dialog"]//span[contains(text(), "Post")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//span[contains(text(), "发布")]/ancestor::div[@role="button"][1]`,
		`//div[@role="dialog"]//span[contains(text(), "نشر")]/ancestor::div[@role="button"][1]`,
	}

	logger.Print("FB6", "等待发布按钮可用(最多30秒)...")
	// 使用一个新的 timeout context 进行轮询
	waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
	defer cancelWait()

	var foundDialogSelector string
	_ = time.Now()

	// 轮询检查
Loop:
	for {
		select {
		case <-waitCtx.Done():
			break Loop
		default:
			for _, sel := range dialogPostSelectors {
				// 检查是否存在且不含 aria-disabled="true"
				// Facebook 按钮 disabled 时通常有 aria-disabled="true" 属性
				checkSel := sel + `[not(@aria-disabled="true")]`

				// 快速检查
				checkStepCtx, cancelCheck := context.WithTimeout(ctx, 1*time.Second)
				var nodes []*cdp.Node
				_ = chromedp.Run(checkStepCtx, chromedp.Nodes(checkSel, &nodes, chromedp.BySearch))
				cancelCheck()

				if len(nodes) > 0 {
					foundDialogSelector = checkSel
					break Loop
				}
			}
			time.Sleep(2 * time.Second)
		}
	}

	if foundDialogSelector != "" {
		logger.Print("FB6", "找到弹窗发布按钮: "+foundDialogSelector)
		// 点击
		if err := chromedp.Run(ctx, chromedp.Click(foundDialogSelector, chromedp.BySearch)); err != nil {
			return fmt.Errorf("FB6 %v", err)
		}
		logger.Print("FB6", "已点击发布按钮")
		// 等待弹窗消失或提示出现，这里简单等待一下确保请求发出
		time.Sleep(5 * time.Second)
		return nil
	}

	logger.Print("FB6", "未找到可用的弹窗发布按钮，尝试通用逻辑(Next -> Publish)")

	nextCandidates := []string{
		`//div[@role='button' and (contains(@aria-label,'Next') or contains(.,'Next') or contains(.,'下一步'))]`,
		`//span[normalize-space()='Next']/ancestor::div[@role='button'][1]`,
		`//span[normalize-space()='下一步']/ancestor::div[@role='button'][1]`,
	}

	publishCandidates := []string{
		`//div[@role='button' and (contains(@aria-label,'Publish') or contains(.,'Publish') or contains(.,'发布') or contains(.,'分享') or contains(.,'Post') or contains(.,'发布 Reels') or contains(.,'Share'))]`,
		`//span[normalize-space()='Publish']/ancestor::div[@role='button'][1]`,
		`//span[normalize-space()='发布']/ancestor::div[@role='button'][1]`,
		`//span[normalize-space()='Share']/ancestor::div[@role='button'][1]`,
		`//span[normalize-space()='分享']/ancestor::div[@role='button'][1]`,
		`//div[@role='button' and contains(.,'Post')]`,
	}

	for i := 0; i < 3; i++ {
		clicked, _ := clickFirstFound(ctx, nextCandidates)
		if !clicked {
			break
		}
		logger.Print("FB6", "已点击Next")
		time.Sleep(1500 * time.Millisecond)
	}

	clicked, err := clickFirstFound(ctx, publishCandidates)
	if err != nil {
		return fmt.Errorf("FB6 %v", err)
	}
	if !clicked {
		return errors.New("FB6 cannot find publish button on facebook page")
	}
	logger.Print("FB6", "已点击发布按钮")
	return nil
}

func clickFirstFound(ctx context.Context, xpaths []string) (bool, error) {
	for _, xp := range xpaths {
		stepCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var nodes []*cdp.Node
		_ = chromedp.Run(stepCtx, chromedp.Nodes(xp, &nodes, chromedp.BySearch))
		cancel()
		if len(nodes) == 0 {
			continue
		}
		clickCtx, cancelClick := context.WithTimeout(ctx, 15*time.Second)
		err := chromedp.Run(clickCtx, chromedp.Click(xp, chromedp.BySearch))
		cancelClick()
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
