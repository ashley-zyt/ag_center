package douyin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"minimax_pro/internal/chromedputil"
	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
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

// PublishRequest 抖音发布请求参数
type PublishRequest struct {
	WebsocketURL     string
	Text             string
	VideoPath        string // 本地视频文件路径
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

// PublishVideo 连接远程浏览器，打开抖音上传页，上传视频、填写标题，点击发布，
// 然后等待并关闭浏览器，最终删除本地视频文件
func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	if req.WebsocketURL == "" {
		return errors.New("DY0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("DY0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("DY0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("DY0 %v", err)
	}

	logger.Print("DY1", "连接浏览器WebSocket")
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, _ := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
		chromedp.WithErrorf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
	)

	// 清理多余标签页
	chromedputil.CleanExtraTabs(tabCtx, logger, "DY1")

	defer func() {
		logger.Print("DY7", "关闭标签页")
		_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			closeTabCtx, cancelCloseTab := context.WithTimeout(ctx, 6*time.Second)
			defer cancelCloseTab()
			var result interface{}
			return chromedp.Run(closeTabCtx, chromedp.Evaluate(`window.close()`, &result))
		}))

		logger.Print("DY7", "关闭所有标签页")
		closeCtx, cancelClose := context.WithTimeout(tabCtx, 10*time.Second)
		if err := chromedputil.CloseAllTabsThenBrowser(closeCtx); err != nil {
			logger.Print("DY7", "关闭标签页失败: "+err.Error())
		} else {
			logger.Print("DY7", "已关闭所有标签页")
		}
		cancelClose()

		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancelStop()
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			logger.Print("DY7", "已请求停止Undetectable Profile")
		}
		logger.Print("DY7", "资源清理完成")
	}()

	// 整体超时 15 分钟：覆盖慢网速下的多次重新上传重试 + 验证码人工处理等待
	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 15*time.Minute)
	defer cancelTimeout()

	// 打开抖音创作者中心上传页
	if err := chromedp.Run(tabCtx, chromedp.Navigate("https://creator.douyin.com/creator-micro/content/upload"), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return fmt.Errorf("DY2 %v", err)
	}
	logger.Print("DY2", "已打开抖音创作者中心上传页")

	// 检查是否需要登录
	loginCheckCtx, cancelLoginCheck := context.WithTimeout(tabCtx, 5*time.Second)
	var loginNodes []*cdp.Node
	_ = chromedp.Run(loginCheckCtx, chromedp.Nodes(`//*[contains(text(), '登录') or contains(text(), '扫码登录') or contains(text(), '手机号登录') or contains(text(), '请登录')]`, &loginNodes, chromedp.BySearch))
	cancelLoginCheck()
	if len(loginNodes) > 0 {
		// 进一步确认是登录页面而非普通弹窗
		urlCheckCtx, cancelUrlCheck := context.WithTimeout(tabCtx, 3*time.Second)
		var href string
		_ = chromedp.Run(urlCheckCtx, chromedp.Location(&href))
		cancelUrlCheck()
		if strings.Contains(href, "login") || strings.Contains(href, "passport") {
			return errors.New("DY2 douyin not logged in in this profile")
		}
	}

	// 前置检查账号异常/功能不可用
	anomalyCheckCtx, cancelAnomalyCheck := context.WithTimeout(tabCtx, 4*time.Second)
	var anomalyNodes []*cdp.Node
	_ = chromedp.Run(anomalyCheckCtx, chromedp.Nodes(`//*[contains(text(), '账号异常') or contains(text(), '功能受限') or contains(text(), '暂时无法') or contains(text(), '违规')]`, &anomalyNodes, chromedp.BySearch))
	cancelAnomalyCheck()
	if len(anomalyNodes) > 0 {
		return errors.New("DY2 douyin account anomaly pre-check: account restricted")
	}

	// 上传视频文件
	if err := waitAndUploadFile(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("DY3 %v", err)
	}

	// 关闭上传后的提示弹窗（缩短超时，快速退出）
	_ = dismissPopups(tabCtx, logger)

	// 智能等待视频上传完成（进度条消失 + 封面出现；上传失败时自动点击"重新上传"）
	if err := waitForUploadComplete(tabCtx, logger, absVideoPath); err != nil {
		return fmt.Errorf("DY4 %v", err)
	}

	// 上传完成后会弹出"视频预览功能"等引导弹窗，需再次关闭（否则遮挡文案输入框和发布按钮）
	_ = dismissPopups(tabCtx, logger)

	// 填写视频描述/文案
	if req.Text != "" {
		if err := fillText(tabCtx, logger, req.Text); err != nil {
			return fmt.Errorf("DY5 %v", err)
		}
	}
	logger.Print("DY6", "已填写标题，等待点击发布")

	// 短暂等待确保视频处理完成后再点击发布（封面已出现时无需长等待）
	time.Sleep(5 * time.Second)

	if err := clickPublish(tabCtx, logger); err != nil {
		return fmt.Errorf("DY6 %v", err)
	}

	// 等待页面跳转确认发布成功
	redirectOK := false
	verificationHandled := false
	for attempt := 1; attempt <= 2; attempt++ {
		logger.Print("DY6", fmt.Sprintf("已点击发布，等待页面跳转 (第%d次)", attempt))
		redirectDeadline := time.Now().Add(120 * time.Second)
		redirectOK = false
		for time.Now().Before(redirectDeadline) {
			// 检测是否出现短信验证码弹窗
			if !verificationHandled {
				verifyCtx, cancelVerify := context.WithTimeout(tabCtx, 2*time.Second)
				var verifyNodes []*cdp.Node
				_ = chromedp.Run(verifyCtx, chromedp.Nodes(`//*[contains(text(), '验证码') or contains(text(), '短信验证') or contains(text(), '手机验证') or contains(text(), '安全验证')]`, &verifyNodes, chromedp.BySearch))
				cancelVerify()
				if len(verifyNodes) > 0 {
					logger.Print("DY6", "⚠️ 检测到短信验证码弹窗，请在浏览器中手动输入验证码并完成验证")
					logger.Print("DY6", "等待人工处理验证码... (最长等待3分钟)")
					verificationHandled = true
					// 等待验证码弹窗消失(用户手动处理)
					verifyDeadline := time.Now().Add(3 * time.Minute)
					for time.Now().Before(verifyDeadline) {
						time.Sleep(3 * time.Second)
						checkCtx, cancelCheck := context.WithTimeout(tabCtx, 2*time.Second)
						var stillNodes []*cdp.Node
						_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[contains(text(), '验证码') or contains(text(), '短信验证') or contains(text(), '手机验证') or contains(text(), '安全验证')]`, &stillNodes, chromedp.BySearch))
						cancelCheck()
						if len(stillNodes) == 0 {
							logger.Print("DY6", "验证码弹窗已消失，继续等待页面跳转")
							break
						}
					}
				}
			}

			var href string
			locCtx, cancelLoc := context.WithTimeout(tabCtx, 1500*time.Millisecond)
			_ = chromedp.Run(locCtx, chromedp.Location(&href))
			cancelLoc()
			if strings.Contains(href, "content/manage") || strings.Contains(href, "content/publish") {
				redirectOK = true
				break
			}
			time.Sleep(800 * time.Millisecond)
		}
		if redirectOK {
			break
		}
		if attempt < 2 {
			logger.Print("DY6", "超时未跳转，重试点击发布")
			if err := clickPublish(tabCtx, logger); err != nil {
				logger.Print("DY6", "重试点击发布失败: "+err.Error())
				break
			}
		}
	}
	if !redirectOK {
		// 兜底：检查页面是否出现发布成功的提示
		successCheckCtx, cancelSuccess := context.WithTimeout(tabCtx, 3*time.Second)
		var successNodes []*cdp.Node
		_ = chromedp.Run(successCheckCtx, chromedp.Nodes(`//*[contains(text(), '发布成功') or contains(text(), '作品发布成功')]`, &successNodes, chromedp.BySearch))
		cancelSuccess()
		if len(successNodes) > 0 {
			logger.Print("DY6", "检测到发布成功提示，判定发布成功")
			redirectOK = true
		}
	}
	if !redirectOK {
		logger.Print("DY6", "重试后仍未跳转，未知原因需要人为检查")
		time.Sleep(8 * time.Second)
		return errors.New("DY6 未跳转至抖音内容管理页，未知原因需要人为检查")
	}
	logger.Print("DY6", "发布成功")
	time.Sleep(8 * time.Second)
	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("DY8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("DY8", "已删除本地视频: "+absVideoPath)
	}
	return nil
}

// waitAndUploadFile 等待文件上传控件并选择本地视频
func waitAndUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("DY3", "等待视频上传控件")
	uploadSelectors := []string{
		`//input[@type='file']`,
		`//div[contains(@class,'upload')]//input[@type='file']`,
		`//div[contains(@class,'container')]//input[@type='file']`,
		`//input[@accept='video/*']`,
	}
	var found string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		// 动态监测页面是否突发账号异常
		anomalyCtx, cancelAnomaly := context.WithTimeout(ctx, 2*time.Second)
		var anomalyNodes []*cdp.Node
		_ = chromedp.Run(anomalyCtx, chromedp.Nodes(`//*[contains(text(), '账号异常') or contains(text(), '功能受限') or contains(text(), '暂时无法') or contains(text(), '违规')]`, &anomalyNodes, chromedp.BySearch))
		cancelAnomaly()
		if len(anomalyNodes) > 0 {
			return errors.New("DY3 douyin account anomaly: account restricted")
		}

		for _, sel := range uploadSelectors {
			checkCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
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
		return errors.New("DY3 video upload control not found within 60 seconds")
	}

	logger.Print("DY3", "使用选择器: "+found)

	uploadCtx, cancelUpload := context.WithTimeout(ctx, 20*time.Second)
	defer cancelUpload()

	if err := chromedp.Run(uploadCtx, chromedp.WaitReady(found, chromedp.BySearch)); err != nil {
		return checkAnomalyContext(ctx, fmt.Errorf("DY3 wait upload input ready failed: %v", err))
	}

	logger.Print("DY4", "开始选择视频文件: "+absVideoPath)
	if err := chromedp.Run(uploadCtx, chromedp.SetUploadFiles(found, []string{absVideoPath}, chromedp.BySearch)); err != nil {
		return checkAnomalyContext(ctx, fmt.Errorf("DY4 set upload files failed: %v", err))
	}
	return nil
}

// uploadStateJS 页面上传状态探测脚本：返回 {state: failed/uploading/done/waiting, speed: 当前上传速度}
const uploadStateJS = `(function(){
	var body = document.body ? (document.body.innerText || "") : "";
	// 单位换算 → MB（页面进度文本如 "已上传: 0.9MB/5.1MB"）
	function toMB(v, u){
		u = (u || "").toUpperCase();
		if(u.indexOf("T") === 0) return v * 1024 * 1024;
		if(u.indexOf("G") === 0) return v * 1024;
		if(u.indexOf("M") === 0) return v;
		if(u.indexOf("K") === 0) return v / 1024;
		return v / 1024 / 1024;
	}
	// 当前上传速度（如 "13.5KB/s"），仅上传中时页面才显示"当前速度"行
	var speed = "";
	var sm = body.match(/当前速度[:：]\s*([0-9.]+\s*[KMGT]?B\/s)/);
	if(sm) speed = sm[1];
	// 上传进度（如 "已上传: 0.9MB/5.1MB"），用于估算剩余上传时间、智能延长等待截止时间
	var uploadedMB = 0, totalMB = 0;
	var pm = body.match(/已上传[:：]?\s*([0-9.]+)\s*([KMGT]?B)\s*\/\s*([0-9.]+)\s*([KMGT]?B)/);
	if(pm){ uploadedMB = toMB(parseFloat(pm[1]), pm[2]); totalMB = toMB(parseFloat(pm[3]), pm[4]); }
	var state = "waiting";
	// 上传失败判定（网速慢时抖音会显示"上传失败，重新上传"），需优先于上传中/完成判定
	if(body.indexOf("上传失败") >= 0) state = "failed";
	// 上传中判定：仅依赖明确的上传进度文本（注意：不要用 class*="progress"，会误中视频播放器进度条）
	else if(body.indexOf("上传过程中请不要删除") >= 0) state = "uploading";
	else if(body.indexOf("正在上传") >= 0) state = "uploading";
	else {
		var m = body.match(/\u4e0a\u4f20[^\n]*?(\d{1,3})%/);
		if(m && m[1] !== "100") state = "uploading";
		// 完成判定：上传完成后才会出现的页面元素（实测抖音创作者中心）
		else if(body.indexOf("设置封面") >= 0) state = "done";
		else if(body.indexOf("选择封面") >= 0) state = "done";
		else if(body.indexOf("重新上传") >= 0) state = "done";
		else if(body.indexOf("更换封面") >= 0) state = "done";
		else if(body.indexOf("作品描述") >= 0 && body.indexOf("发布时间") >= 0) state = "done";
		else {
			// 发布按钮已存在（上传完成且视频处理完毕）
			var publishBtn = document.querySelector('#popover-tip-container > button, button[class*="publish"]');
			if(publishBtn) state = "done";
		}
	}
	return {state: state, speed: speed, uploadedMB: uploadedMB, totalMB: totalMB};
})()`

// uploadStateInfo 页面上传状态探测结果
type uploadStateInfo struct {
	State      string  `json:"state"`      // failed / uploading / done / waiting
	Speed      string  `json:"speed"`      // 当前上传速度（如 "13.5KB/s"），仅上传中才有值
	UploadedMB float64 `json:"uploadedMB"` // 已上传大小（MB），来自页面"已上传: x/y"
	TotalMB    float64 `json:"totalMB"`    // 文件总大小（MB）
}

// fetchUploadState 读取一次页面上传状态与当前上传速度（读取失败时 State 为空）
func fetchUploadState(ctx context.Context) uploadStateInfo {
	var info uploadStateInfo
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = chromedp.Run(checkCtx, chromedp.Evaluate(uploadStateJS, &info))
	return info
}

// checkUploadState 读取一次页面上传状态（读取失败时返回空字符串）
func checkUploadState(ctx context.Context) string {
	return fetchUploadState(ctx).State
}

// parseSpeedKBps 解析页面速度文本（如 "13.5KB/s"、"1.2MB/s"）为 KB/s 数值
func parseSpeedKBps(s string) (float64, bool) {
	var v float64
	var unit string
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f%s", &v, &unit); err != nil {
		return 0, false
	}
	unit = strings.ToUpper(strings.TrimSuffix(strings.ToUpper(unit), "/S"))
	switch {
	case strings.HasPrefix(unit, "T"):
		return v * 1024 * 1024 * 1024, true
	case strings.HasPrefix(unit, "G"):
		return v * 1024 * 1024, true
	case strings.HasPrefix(unit, "M"):
		return v * 1024, true
	case strings.HasPrefix(unit, "K"):
		return v, true
	case unit == "B" || unit == "":
		return v / 1024, true
	}
	return 0, false
}

// formatSpeed 将 KB/s 数值格式化为易读速度文本
func formatSpeed(kbps float64) string {
	switch {
	case kbps >= 1024*1024:
		return fmt.Sprintf("%.2fGB/s", kbps/(1024*1024))
	case kbps >= 1024:
		return fmt.Sprintf("%.2fMB/s", kbps/1024)
	case kbps >= 1:
		return fmt.Sprintf("%.1fKB/s", kbps)
	default:
		return fmt.Sprintf("%.0fB/s", kbps*1024)
	}
}

// avgSpeedText 由速度样本计算平均速度文本（无样本时返回空字符串）
func avgSpeedText(samples []float64) string {
	if len(samples) == 0 {
		return ""
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return "，平均上传速度: " + formatSpeed(sum/float64(len(samples)))
}

// hasPositiveSample 判断速度样本中是否存在大于 0 的速度（用于区分"有网速"与"速度一直为0"）
func hasPositiveSample(samples []float64) bool {
	for _, v := range samples {
		if v > 0 {
			return true
		}
	}
	return false
}

// waitUploadStateLeave 轮询等待上传状态脱离 from（如脱离 failed），返回最新状态及是否脱离成功
func waitUploadStateLeave(ctx context.Context, from string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	last := from
	for time.Now().Before(deadline) {
		if s := checkUploadState(ctx); s != "" {
			last = s
			if s != from {
				return s, true
			}
		}
		time.Sleep(2 * time.Second)
	}
	return last, false
}

// clickRetryUpload 点击页面上的"重新上传"（优先真实鼠标事件，JS 兜底）
func clickRetryUpload(ctx context.Context, logger *logx.Logger) bool {
	realCtx, cancelReal := context.WithTimeout(ctx, 8*time.Second)
	err := chromedp.Run(realCtx,
		chromedp.ScrollIntoView(`//*[contains(text(),'重新上传')]`, chromedp.BySearch),
		chromedp.Click(`//*[contains(text(),'重新上传')]`, chromedp.BySearch),
	)
	cancelReal()
	if err == nil {
		logger.Print("DY4", "已用真实鼠标事件点击'重新上传'")
		return true
	}

	// JS 兜底点击（取文档序中最后一个匹配元素即最深节点，避免误点外层容器）
	var jsClicked bool
	retryCtx, cancelRetry := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(retryCtx, chromedp.Evaluate(`(function(){
		var els = document.querySelectorAll('button, div[role="button"], a, span, p, div');
		var target = null;
		for(var i=0;i<els.length;i++){
			var t = (els[i].innerText||"").trim();
			if(t.indexOf("重新上传") >= 0 && t.length < 30){ target = els[i]; }
		}
		if(target){ try{target.click();}catch(e){} return true; }
		return false;
	})()`, &jsClicked))
	cancelRetry()
	if jsClicked {
		logger.Print("DY4", "已用 JS 兜底点击'重新上传'")
		return true
	}
	logger.Print("DY4", "未找到可点击的'重新上传'元素")
	return false
}

// injectUploadFile 向上传控件重新注入视频文件（优先 accept 含 video 的 input）。
// 注意：返回 true 仅代表 CDP 调用成功，是否真正触发重传必须由调用方通过状态变化校验。
func injectUploadFile(ctx context.Context, logger *logx.Logger, absVideoPath string) bool {
	selectors := []string{
		`//input[@type='file' and contains(@accept,'video')]`,
		`//input[@type='file']`,
	}
	for _, sel := range selectors {
		injectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(injectCtx, chromedp.SetUploadFiles(sel, []string{absVideoPath}, chromedp.BySearch))
		cancel()
		if err == nil {
			logger.Print("DY4", "已重新注入视频文件（选择器: "+sel+"）")
			return true
		}
		logger.Print("DY4", "注入尝试失败（选择器: "+sel+"）: "+err.Error())
	}
	return false
}

// retryViaFileChooser 点击"重新上传"并接管其唤起的原生文件选择框。
// 旧实现用真实鼠标点击唤起系统文件选择对话框后，再用 SetUploadFiles 绕过对话框注入文件，
// 导致原生对话框无人关闭、一直残留在屏幕上。这里改为：先用 CDP 拦截文件选择框请求（拦截后系统对话框根本不会弹出），
// 再按 fileChooserOpened 事件携带的 backendNodeId 对触发对话框的那个 input 注入文件，选择请求被接受后自动完成。
// 返回 true 表示已成功接管并完成文件选择（上传是否恢复由调用方校验状态）。
func retryViaFileChooser(ctx context.Context, logger *logx.Logger, absVideoPath string) bool {
	enableCtx, cancelEnable := context.WithTimeout(ctx, 5*time.Second)
	err := chromedp.Run(enableCtx, page.SetInterceptFileChooserDialog(true))
	cancelEnable()
	if err != nil {
		logger.Print("DY4", "开启文件选择框拦截失败: "+err.Error())
		return false
	}
	defer func() {
		disableCtx, cancelDisable := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(disableCtx, page.SetInterceptFileChooserDialog(false))
		cancelDisable()
	}()

	handled := make(chan bool, 1)
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen() // 取消即移除事件监听器，避免影响后续流程（如首次上传）
	// 注意：监听回调中不能同步执行 CDP 调用（会死锁），必须另起 goroutine
	chromedp.ListenTarget(listenCtx, func(ev interface{}) {
		e, ok := ev.(*page.EventFileChooserOpened)
		if !ok {
			return
		}
		backendNodeID := e.BackendNodeID
		go func() {
			if backendNodeID == 0 {
				logger.Print("DY4", "文件选择框事件未携带控件节点，无法接管")
				select {
				case handled <- false:
				default:
				}
				return
			}
			params := dom.SetFileInputFiles([]string{absVideoPath})
			params.BackendNodeID = backendNodeID // 精确定位触发对话框的 input，避免误注入其他 file input
			handleCtx, cancelHandle := context.WithTimeout(ctx, 10*time.Second)
			err := chromedp.Run(handleCtx, params)
			cancelHandle()
			if err != nil {
				logger.Print("DY4", "接管文件选择框失败: "+err.Error())
				select {
				case handled <- false:
				default:
				}
				return
			}
			logger.Print("DY4", "已接管文件选择框并完成文件选择（对话框不会残留）")
			select {
			case handled <- true:
			default:
			}
		}()
	})

	// 需真实鼠标事件（可信事件）才能唤起原生文件选择框，JS click 不可信可能无法弹出
	realCtx, cancelReal := context.WithTimeout(ctx, 8*time.Second)
	err = chromedp.Run(realCtx,
		chromedp.ScrollIntoView(`//*[contains(text(),'重新上传')]`, chromedp.BySearch),
		chromedp.Click(`//*[contains(text(),'重新上传')]`, chromedp.BySearch),
	)
	cancelReal()
	if err != nil {
		logger.Print("DY4", "未找到可点击的'重新上传'元素，回退直接注入方式")
		return false
	}
	logger.Print("DY4", "已用真实鼠标事件点击'重新上传'，等待接管文件选择框...")

	select {
	case ok := <-handled:
		return ok
	case <-time.After(10 * time.Second):
		logger.Print("DY4", "点击后未唤起原生文件选择框，回退直接注入方式")
		return false
	}
}

// waitForUploadComplete 智能等待视频上传完成。
// 检测条件：上传进度文本消失 + 封面选择区/发布按钮出现。
// 轮询期间主动关闭"视频预览功能"等引导弹窗，防止弹窗遮挡导致流程卡住。
// 网速慢导致"上传失败，重新上传"时：连续2次确认失败后才触发重试（防抖）；
// 重试策略（实测：失败态下直接向 input 注入文件 CDP 返回成功但页面不响应，必须先让组件重置状态）：
//  1. 真实鼠标点击"重新上传"，用 CDP 拦截其唤起的文件选择框并按事件中的 backendNodeId 注入文件
//     （拦截后系统对话框不会实际弹出，修复旧实现中对话框残留不消失的问题），并轮询校验状态脱离 failed；
//  2. 未唤起原生对话框时回退为点击后直接向上传控件注入；
//  3. 仍未恢复则整页刷新，从头重新选择视频上传；
//
// 最多重试5次。等待截止时间智能延长：有速度时按 剩余大小/当前速度 估算延长，速度一直为 0 才固定延长 2 分钟。
func waitForUploadComplete(ctx context.Context, logger *logx.Logger, absVideoPath string) error {
	logger.Print("DY4", "智能等待视频上传完成...")
	deadline := time.Now().Add(5 * time.Minute)
	loopCount := 0
	retryCount := 0
	failedStreak := 0
	var speedSamples []float64 // 上传中采集的速度样本，最终失败时用于计算平均上传速度

	for time.Now().Before(deadline) {
		loopCount++

		// 每轮先尝试关闭引导弹窗（如"视频预览功能"的"知道了"按钮）
		var popupClicked bool
		popupCtx, cancelPopup := context.WithTimeout(ctx, 3*time.Second)
		_ = chromedp.Run(popupCtx, chromedp.Evaluate(`(function(){
			var btns = document.querySelectorAll('button, div[role="button"]');
			for(var i=0;i<btns.length;i++){
				var t = (btns[i].innerText||"").trim();
				if(btns[i].disabled || btns[i].getAttribute('aria-disabled')==='true') continue;
				if(t==='知道了' || t==='我知道了' || t==='不再提示' || t==='知道了，进入预览'){
					try{btns[i].click();}catch(e){}
					return true;
				}
			}
			return false;
		})()`, &popupClicked))
		cancelPopup()
		if popupClicked {
			logger.Print("DY4", "已关闭引导弹窗（如'视频预览功能'提示框）")
			time.Sleep(1 * time.Second)
			continue
		}

		// 一次性获取页面状态：是否上传中 + 是否已完成 + 当前上传速度（单次 JS 调用，减少 CDP 往返）
		stateInfo := fetchUploadState(ctx)
		state := stateInfo.State

		// 上传中持续采集速度样本，用于最终失败时计算平均上传速度（采样间隔均匀，直接取均值即可）
		var curSpeedKBps float64
		if state == "uploading" {
			if v, ok := parseSpeedKBps(stateInfo.Speed); ok {
				speedSamples = append(speedSamples, v)
				curSpeedKBps = v
			}
		}

		// 智能延长等待截止时间：当前速度 > 0 时，按 剩余大小/当前速度 估算剩余时间（另加 2 分钟缓冲应对速度波动），
		// 只延长不缩短，避免慢网速下过早超时；速度一直为 0 时不在此处延长（由重试分支的固定延长兜底）
		if state == "uploading" && curSpeedKBps > 0 && stateInfo.TotalMB > 0 {
			remainingMB := stateInfo.TotalMB - stateInfo.UploadedMB
			if remainingMB < 0 {
				remainingMB = 0
			}
			estSec := remainingMB * 1024 / curSpeedKBps
			newDeadline := time.Now().Add(time.Duration(estSec)*time.Second + 2*time.Minute)
			// 仅在需延长幅度较大(≥15秒)时才调整并记日志，避免速度波动导致频繁刷屏
			if newDeadline.Sub(deadline) > 15*time.Second {
				deadline = newDeadline
				logger.Print("DY4", fmt.Sprintf("按当前速度 %s 估算剩余约 %.0f 秒(已上传 %.1fMB/%.1fMB)，已延长等待截止时间", formatSpeed(curSpeedKBps), estSec, stateInfo.UploadedMB, stateInfo.TotalMB))
			}
		}

		// 非失败状态下重置失败连续计数（防抖用）
		if state != "failed" {
			failedStreak = 0
		}

		if state == "done" {
			logger.Print("DY4", "视频上传完成，检测到封面选择区/发布按钮已就绪")
			return nil
		}

		// 上传失败：防抖确认后重试。
		// 实测教训：失败态下直接对 input 执行 SetUploadFiles 会返回成功，但页面毫无反应（组件不响应），
		// 因此不能把 CDP 调用成功当作注入生效，必须先点击"重新上传"让组件重置状态，再用状态变化校验效果。
		if state == "failed" {
			failedStreak++
			if failedStreak < 2 {
				logger.Print("DY4", "检测到'上传失败'文字，3秒后复查确认（防抖，避免残留状态误判）")
				time.Sleep(3 * time.Second)
				continue
			}
			failedStreak = 0
			retryCount++
			if retryCount > 5 {
				return errors.New("DY4 上传失败，重试5次后仍无法上传" + avgSpeedText(speedSamples))
			}
			logger.Print("DY4", fmt.Sprintf("确认上传失败，开始重试 (第%d次)", retryCount))

			recovered := false

			// 策略1：点击"重新上传"并接管其唤起的文件选择框（CDP 拦截：对话框不再实际弹出，
			// 按事件中的 backendNodeId 对触发控件注入文件即完成选择，修复旧实现中
			// 真实鼠标点击唤起的系统文件选择框一直残留不消失的问题）
			chooserHandled := retryViaFileChooser(ctx, logger, absVideoPath)

			// 兜底：未唤起原生文件选择框 → 点击"重新上传"让组件退出失败态后直接向上传控件注入文件
			if !chooserHandled {
				if clickRetryUpload(ctx, logger) {
					time.Sleep(1500 * time.Millisecond)
				}
				injectUploadFile(ctx, logger, absVideoPath)
			}

			// 效果校验：轮询等待状态脱离 failed（切回 uploading / done 等），不再盲信注入结果
			if newState, ok := waitUploadStateLeave(ctx, "failed", 15*time.Second); ok {
				logger.Print("DY4", "重试生效，上传状态已恢复: "+newState)
				recovered = true
			} else {
				logger.Print("DY4", "点击'重新上传'+注入后状态仍为 failed")
			}

			// 策略2：点击重传仍未恢复 → 整页刷新后从头重新上传（等价于人工刷新页面重新选文件）
			if !recovered {
				logger.Print("DY4", "整页刷新后从头重新上传")
				navCtx, cancelNav := context.WithTimeout(ctx, 40*time.Second)
				err := chromedp.Run(navCtx, chromedp.Reload(), chromedp.WaitReady("body", chromedp.ByQuery))
				cancelNav()
				if err != nil {
					logger.Print("DY4", "整页刷新失败: "+err.Error())
				} else {
					// 等旧页面的上传控件随导航消失，确认文档已被新页面替换，避免把文件注入到即将被卸载的旧文档
					goneCtx, cancelGone := context.WithTimeout(ctx, 30*time.Second)
					_ = chromedp.Run(goneCtx, chromedp.WaitNotPresent(`//input[@type='file']`, chromedp.BySearch))
					cancelGone()

					if upErr := waitAndUploadFile(ctx, logger, absVideoPath); upErr != nil {
						logger.Print("DY4", "刷新后重新选择视频失败: "+upErr.Error())
					} else if newState, ok := waitUploadStateLeave(ctx, "failed", 15*time.Second); ok {
						logger.Print("DY4", "整页刷新后上传已恢复: "+newState)
						recovered = true
					}
				}
			}

			// 重试需要额外时间：已有正速度样本时，由主循环按 剩余大小/当前速度 智能延长截止时间；
			// 仅当速度一直为 0（或未采集到速度）时才使用固定延长 2 分钟兜底（后续观察交给主循环轮询）
			if !hasPositiveSample(speedSamples) {
				deadline = deadline.Add(2 * time.Minute)
			}
			if !recovered {
				logger.Print("DY4", "本轮重试后上传仍未恢复，等待后下轮继续尝试")
			}
			time.Sleep(5 * time.Second)
			continue
		}

		// 每 5 轮(~15秒)输出一次状态，方便排查卡住原因（上传中附带当前网速）
		if loopCount%5 == 0 {
			msg := fmt.Sprintf("上传状态轮询中: state=%s (已等待%ds)", state, loopCount*3)
			if state == "uploading" && stateInfo.Speed != "" {
				msg += ", 当前速度: " + stateInfo.Speed
			}
			logger.Print("DY4", msg)
		}

		time.Sleep(3 * time.Second)
	}

	// 超时后不报错，继续流程（可能是小视频已上传完成但检测未命中）
	logger.Print("DY4", "等待上传超时(5分钟)，继续执行后续流程")
	return nil
}

// dismissPopups 关闭上传过程中的提示弹窗
func dismissPopups(ctx context.Context, logger *logx.Logger) error {
	logger.Print("DY9", "尝试关闭提示窗口")
	candidates := []string{
		// "视频预览功能"引导弹窗（上传完成后弹出），优先点击其"知道了"按钮
		`//div[contains(@class,'popover') or contains(@class,'popup') or contains(@class,'tip') or contains(@class,'guide')]//button[contains(.,'知道了') or contains(.,'我知道了') or contains(.,'不再提示')]`,
		`//div[contains(@class,'modal')]//button[contains(.,'确定') or contains(.,'知道了') or contains(.,'关闭') or contains(.,'取消') or contains(.,'跳过') or contains(.,'我知道了')]`,
		`//div[contains(@class,'dialog')]//button[contains(.,'确定') or contains(.,'知道了') or contains(.,'关闭') or contains(.,'取消') or contains(.,'跳过') or contains(.,'我知道了')]`,
		`//button[contains(.,'知道了') or contains(.,'我知道了') or contains(.,'不再提示')]`,
		`//button[contains(.,'确定') or contains(.,'关闭') or contains(.,'取消') or contains(.,'跳过')]`,
		`//div[@role='button'][contains(.,'知道了') or contains(.,'我知道了') or contains(.,'确定') or contains(.,'关闭')]`,
		`//div[contains(@class,'close')]`,
		`//div[contains(@class,'mask')]//div[contains(@class,'close')]`,
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
			clickCtx, cancelClick := context.WithTimeout(ctx, 6*time.Second)
			err := chromedp.Run(clickCtx, chromedp.ScrollIntoView(xp, chromedp.BySearch), chromedp.WaitVisible(xp, chromedp.BySearch), chromedp.Click(xp, chromedp.BySearch))
			cancelClick()
			if err == nil {
				logger.Print("DY9", "已关闭提示")
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

// fillText 填写视频描述/文案
func fillText(ctx context.Context, logger *logx.Logger, text string) error {
	logger.Print("DY5", "========== 开始填写视频文案 ==========")
	logger.Print("DY5", "待填写文本长度: "+strconv.Itoa(len(text))+" 字符")
	logger.Print("DY5", "文本预览(前100字符): "+text[:min(len(text), 100)])

	time.Sleep(6 * time.Second)
	if checkSomethingWentWrong(ctx) {
		logger.Print("DY5", "【错误】在等待6秒后检测到页面错误")
		return errors.New("DY5 page error detected after initial sleep")
	}

	// 抖音的标题/描述输入框选择器
	titleSelectors := []string{
		`div[contenteditable='true'][data-placeholder]`,
		`div[class*='title'] [contenteditable='true']`,
		`div[class*='desc'] [contenteditable='true']`,
		`div[class*='caption'] [contenteditable='true']`,
		`div[class*='editor'] [contenteditable='true']`,
		`div[contenteditable='true']`,
		`textarea[placeholder*='标题']`,
		`textarea[placeholder*='描述']`,
		`textarea`,
	}

	var foundSel string
	var foundBy chromedp.QueryOption

	// 查找可用的标题输入框
	for _, sel := range titleSelectors {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var nodes []*cdp.Node
		_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery))
		cancel()
		if len(nodes) > 0 {
			foundSel = sel
			foundBy = chromedp.ByQuery
			break
		}
		// 也尝试 BySearch
		checkCtx2, cancel2 := context.WithTimeout(ctx, 3*time.Second)
		_ = chromedp.Run(checkCtx2, chromedp.Nodes(sel, &nodes, chromedp.BySearch))
		cancel2()
		if len(nodes) > 0 {
			foundSel = sel
			foundBy = chromedp.BySearch
			break
		}
	}

	if foundSel == "" {
		// 兜底：使用 JS 查找
		logger.Print("DY5", "未通过选择器找到标题输入框，尝试JS兜底")
		var jsFound bool
		js := `(function(){
			var els = document.querySelectorAll('[contenteditable="true"]');
			for(var i=0;i<els.length;i++){
				var el = els[i];
				if(el.offsetHeight > 0 && el.offsetWidth > 0){
					return true;
				}
			}
			var ta = document.querySelectorAll('textarea');
			for(var i=0;i<ta.length;i++){
				if(ta[i].offsetHeight > 0 && ta[i].offsetWidth > 0){
					return true;
				}
			}
			return false;
		})()`
		jsCtx, cancelJs := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(jsCtx, chromedp.Evaluate(js, &jsFound))
		cancelJs()
		if !jsFound {
			return errors.New("DY5 cannot find douyin title/description input")
		}
		foundSel = `div[contenteditable='true']`
		foundBy = chromedp.ByQuery
	}

	logger.Print("DY5", "定位标题容器: "+foundSel)

	// Step1: 等待元素可见并点击获取焦点
	stepCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	err := chromedp.Run(stepCtx,
		chromedp.WaitVisible(foundSel, foundBy),
		chromedp.Click(foundSel, foundBy),
		chromedp.Focus(foundSel, foundBy),
	)
	cancel()
	if err != nil {
		logger.Print("DY5", "Step1 失败: 未找到标题元素 - "+err.Error())
		return errors.New("DY5 cannot find douyin caption input")
	}
	logger.Print("DY5", "Step1 完成: 已点击并获取焦点")

	time.Sleep(3 * time.Second)

	// Step2: 输入文本
	logger.Print("DY5", "Step2: 输入文本内容")

	// 先尝试全选清空已有内容，然后输入
	selectJs := fmt.Sprintf(`(function(sel){
		var el=document.querySelector(sel);
		if(!el){return false;}
		el.focus();
		try{
			var selection=window.getSelection();
			if(selection){
				selection.removeAllRanges();
				var range=document.createRange();
				range.selectNodeContents(el);
				selection.addRange(range);
			}
		}catch(e){}
		return true;
	})(%q)`, foundSel)

	var selectOk bool
	typeCtx, cancelType := context.WithTimeout(ctx, 10*time.Second)
	err = chromedp.Run(typeCtx,
		chromedp.Click(foundSel, foundBy),
		chromedp.Focus(foundSel, foundBy),
		chromedp.Evaluate(selectJs, &selectOk),
		chromedp.SendKeys(foundSel, text, foundBy),
	)
	cancelType()
	if err != nil || !selectOk {
		if err != nil {
			logger.Print("DY5", "Step2 警告: SendKeys 执行异常 - "+err.Error())
		} else {
			logger.Print("DY5", "Step2 警告: JavaScript 全选失败")
		}
		// 兜底：使用 JS insertText
		logger.Print("DY5", "Step2: 尝试使用 JavaScript 插入文本")
		var inputOk bool
		inputJs := fmt.Sprintf(`(function(T){
			var el=document.querySelector(%q);
			if(!el){return false;}
			el.focus();
			try{document.execCommand('selectAll', false, null);}catch(e){}
			try{document.execCommand('insertText', false, T);}catch(e){}
			return true;
		})(%q)`, foundSel, text)
		jsCtx, cancelJs := context.WithTimeout(ctx, 5*time.Second)
		if err := chromedp.Run(jsCtx, chromedp.Evaluate(inputJs, &inputOk)); err != nil {
			cancelJs()
			logger.Print("DY5", "Step2 失败: JavaScript 插入也失败 - "+err.Error())
			return errors.New("DY5 cannot input text via SendKeys or JavaScript")
		}
		cancelJs()
		if !inputOk {
			logger.Print("DY5", "Step2 失败: JavaScript 插入返回 false")
			return errors.New("DY5 JavaScript input returned false")
		}
		logger.Print("DY5", "Step2 完成: 使用 JavaScript 插入文本成功")
	} else {
		logger.Print("DY5", "Step2 完成: 使用 JavaScript 全选 + SendKeys 输入成功")
	}

	// 验证最终文本
	var finalText string
	checkCtx, cancelCheck := context.WithTimeout(ctx, 3*time.Second)
	chromedp.Run(checkCtx, chromedp.Evaluate(fmt.Sprintf(`(function(sel){
		var el=document.querySelector(sel);
		return el ? el.textContent : '';
	})(%q)`, foundSel), &finalText))
	cancelCheck()
	logger.Print("DY5", "Step2: 最终文本内容(前100字符): "+finalText[:min(len(finalText), 100)])

	time.Sleep(3 * time.Second)

	if checkSomethingWentWrong(ctx) {
		logger.Print("DY5", "【错误】在输入后检测到页面错误")
		return errors.New("DY5 page error detected after input")
	}

	logger.Print("DY5", "========== 填写视频文案完成 ==========")
	return nil
}

// clickPublish 查找并点击"发布"按钮
func clickPublish(ctx context.Context, logger *logx.Logger) error {
	logger.Print("DY6", "查找发布按钮")
	type selEntry struct {
		s  string
		by chromedp.QueryOption
	}
	sels := []selEntry{
		{`#popover-tip-container > button:not([disabled])`, chromedp.ByQuery},
		// {`//button[contains(.,'发布') and not(@disabled)]`, chromedp.BySearch},
		// {`//div[@role='button'][contains(.,'发布') and not(contains(@class,'disabled'))]`, chromedp.BySearch},
		// {`//button[contains(@class,'publish') and not(@disabled)]`, chromedp.BySearch},
		// {`//button[contains(@class,'submit') and not(@disabled)]`, chromedp.BySearch},
		// {`//span[contains(text(),'发布')]/ancestor::button[1][not(@disabled)]`, chromedp.BySearch},
		// {`//span[contains(text(),'发布')]/ancestor::div[@role='button'][1]`, chromedp.BySearch},
		// {`//button[contains(text(),'Post') and not(@disabled)]`, chromedp.BySearch},
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
			logger.Print("DY6", "找到发布按钮: "+e.s)
			clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
			err := chromedp.Run(clickCtx,
				chromedp.ScrollIntoView(e.s, e.by),
				chromedp.WaitVisible(e.s, e.by),
				chromedp.Click(e.s, e.by),
			)
			cancelClick()

			if err == nil {
				logger.Print("DY6", "已点击发布按钮")
				return nil
			}

			// JS 兜底点击
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
				logger.Print("DY6", "通过 JS 兜底点击发布按钮成功")
				return nil
			}
		}
		time.Sleep(800 * time.Millisecond)
	}
	return errors.New("DY6 cannot find publish button on douyin page")
}

func checkAnomalyContext(ctx context.Context, originalErr error) error {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var nodes []*cdp.Node
	_ = chromedp.Run(checkCtx, chromedp.Nodes(`//*[contains(text(), '账号异常') or contains(text(), '功能受限') or contains(text(), '暂时无法') or contains(text(), '违规')]`, &nodes, chromedp.BySearch))
	if len(nodes) > 0 {
		return errors.New("DY3 douyin account anomaly: account restricted interrupted the upload process")
	}
	return originalErr
}

func checkSomethingWentWrong(ctx context.Context) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var hasError bool
	checkJs := `(function(){
		var body = document.body ? (document.body.innerText || "") : "";
		if(body.indexOf('页面出错') >= 0 || body.indexOf('加载失败') >= 0 || body.indexOf('请刷新') >= 0){
			return true;
		}
		return false;
	})()`

	_ = chromedp.Run(checkCtx, chromedp.Evaluate(checkJs, &hasError))
	return hasError
}
