package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

type filterLogger struct {
	logger *logx.Logger
}

// uploadNetSignal 网络层上传启动信号：出现腾讯上传分片请求（applyuploaddfs/uploadpartdfs 等）后置 1。
// 页面文案信号（"正在上传"/进度）在指纹浏览器内核下可能永远不渲染，网络信号是唯一可靠判据；
// 避免仅凭文案误判"未启动"而重复注入文件（曾触发"切换"弹窗并卡死上传状态机，发表按钮永远灰色）。
var uploadNetSignal int32

func isUploadNetURL(u string) bool {
	return strings.Contains(u, "applyuploaddfs") || strings.Contains(u, "uploadpartdfs") || strings.Contains(u, "completepartuploaddfs")
}

// publishNetSignal 发布成功网络信号：post_create 请求发出即置 1（服务端受理发表）。
// 比页面跳转/文案更可靠：指纹浏览器下 wujie 子应用 URL 可能不变，仅凭 URL 判转会漏判（实测曾发布成功却傻等重试）。
var publishNetSignal int32

func isPublishNetURL(u string) bool {
	return strings.Contains(u, "/post/post_create")
}

func (l *filterLogger) Printf(format string, v ...interface{}) {
	msg := ""
	if len(v) > 0 {
		msg = fmt.Sprintf(format, v...)
	} else {
		msg = format
	}
	if strings.Contains(msg, "could not unmarshal event: unknown PrivateNetworkRequestPolicy value") ||
		strings.Contains(msg, "could not unmarshal event: unknown ClientNavigationReason value") ||
		strings.Contains(msg, "could not unmarshal event: unknown IPAddressSpace value") {
		return
	}
	// 输出 chromedp 内部日志/错误（如 ws 断连、目标销毁等关键线索）
	l.logger.Print("WXCDP", msg)
}

// PublishRequest 微信视频号发布请求参数
type PublishRequest struct {
	WebsocketURL     string
	Text             string
	VideoPath        string // 本地视频文件路径
	UndetectableHost string
	UndetectablePort int
	ProfileID        string
}

// PublishVideo 连接远程浏览器，打开微信视频号助手发表页，上传视频、填写描述，点击发表，
// 然后等待并关闭浏览器，最终删除本地视频文件
func PublishVideo(ctx context.Context, logger *logx.Logger, req PublishRequest) error {
	// 包级信号跨请求会残留（服务常驻），每次发布开始必须重置，否则第二次请求会误判"上传已启动"
	atomic.StoreInt32(&uploadNetSignal, 0)
	atomic.StoreInt32(&publishNetSignal, 0)
	if req.WebsocketURL == "" {
		return errors.New("WX0 websocket_url is required")
	}
	if req.VideoPath == "" {
		return errors.New("WX0 video_path is required")
	}
	absVideoPath, err := filepath.Abs(req.VideoPath)
	if err != nil {
		return fmt.Errorf("WX0 %v", err)
	}
	if _, err := os.Stat(absVideoPath); err != nil {
		return fmt.Errorf("WX0 %v", err)
	}

	logger.Print("WX1", "连接浏览器WebSocket")
	httpBase := httpBaseFromWS(req.WebsocketURL)
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, req.WebsocketURL, chromedp.NoModifyURL)
	defer cancelAlloc()
	logOpts := []chromedp.ContextOption{
		chromedp.WithLogf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
		chromedp.WithErrorf(func(format string, v ...interface{}) { (&filterLogger{logger: logger}).Printf(format, v...) }),
	}
	tabCtx, _ := chromedp.NewContext(allocCtx, logOpts...)

	// 新建自己的标签页（首次 Run 触发 CreateTarget + attach），该调用也初始化了 target 会话。
	// 不复用/附着浏览器中已有的标签页：chromedp 在上下文取消时会自动 CloseTarget 所附着的标签，
	// 会误关用户标签；若那是最后一个标签还会导致整个浏览器退出（实测）。
	//
	// 关键：首次 Run 绝不能包 WithTimeout/短命 context！RemoteAllocator.Allocate 内部会派生一个
	// goroutine 监听首次 Run 传入 ctx 的 Done，一旦该 ctx 结束就 Cancel 整个 chromedp 上下文并关闭
	// ws 连接（chromedp 官方注释同样警告：不要在首次 Run 上加超时）。超时保护交给拨号自带的10秒。
	err = chromedp.Run(tabCtx, chromedp.ActionFunc(func(context.Context) error { return nil }))
	if err != nil {
		logger.Print("WX1", "初始化标签页失败: "+err.Error())
		logCancelDiagnostics(httpBase, tabCtx, allocCtx, ctx, logger, "初始化标签页失败")
	}
	if tc := chromedp.FromContext(tabCtx); tc != nil && tc.Target != nil {
		logger.Print("WX1", "已创建新标签页: "+string(tc.Target.TargetID))
	}

	// 注册控制台/JS异常监听：视频号助手发表区由 wujie 微前端子应用 JS 渲染，
	// 子应用挂载失败时的报错是定位渲染不出来的关键线索（网络诊断在导航后启用）
	registerConsoleDiagnostics(tabCtx, logger)

	// 自动接受页面 JS 弹窗：JS 弹窗（alert/confirm/beforeunload）会阻塞整个渲染器的 JS 执行，
	// 导致 load 事件不触发、所有 CDP 命令挂起（实测曾导致导航后全部命令超时）
	setupDialogHandler(tabCtx, logger)

	// 导航前注入全局错误收集器：wujie 子应用挂载失败时仅通过 console.error(Event) 暴露（实测只拿到裸 Event），
	// 该脚本在任何页面 JS 之前运行，把 error/unhandledrejection/console.error 的详情(含 Event 展开)存入 window.__WXERRLOG
	insCtx, cancelIns := context.WithTimeout(tabCtx, 5*time.Second)
	if err := chromedp.Run(insCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, e := page.AddScriptToEvaluateOnNewDocument(errCollectorScript).Do(c)
		return e
	})); err != nil {
		logger.Print("WX2", "注入错误收集器失败(不影响主流程): "+err.Error())
	}
	cancelIns()

	// 焦点仿真：让页面始终认为自己处于聚焦状态，避免标签页失焦时子应用初始化被挂起/跳过（微信助手实测相关）
	feCtx, cancelFe := context.WithTimeout(tabCtx, 5*time.Second)
	if err := chromedp.Run(feCtx, emulation.SetFocusEmulationEnabled(true)); err != nil {
		logger.Print("WX2", "启用焦点仿真失败(不影响主流程): "+err.Error())
	}
	cancelFe()

	// 导航前先激活标签页：wujie 子应用在 visibilityState=hidden 时会推迟挂载/渲染（实测子应用 JS 已运行、
	// 业务接口已请求，但 shadow DOM 始终无投影），必须保证导航开始时标签页处于前台可见状态。
	preAfCtx, cancelPreAf := context.WithTimeout(tabCtx, 5*time.Second)
	if err := chromedp.Run(preAfCtx, page.BringToFront()); err != nil {
		logger.Print("WX2", "导航前激活标签页失败(不影响主流程): "+err.Error())
	}
	cancelPreAf()

	defer func() {
		// 只关闭本次自己创建的标签页，不影响浏览器中的其他标签。
		// 优先用 HTTP 接口（不依赖可能已失效的 CDP 会话）；
		// 注意：即使这里不关，上下文取消时 chromedp 也会自动 CloseTarget 我们自己的标签，两者不冲突。
		logger.Print("WX7", "关闭本次创建的标签页")
		var myID target.ID
		if tc := chromedp.FromContext(tabCtx); tc != nil && tc.Target != nil {
			myID = tc.Target.TargetID
		}
		if myID != "" {
			closeTabViaHTTP(httpBase, string(myID), logger)
		}

		if req.ProfileID != "" && req.UndetectableHost != "" && req.UndetectablePort != 0 {
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancelStop()
			_ = undetectable.NewClient(req.UndetectableHost, req.UndetectablePort).StopProfileBestEffort(stopCtx, req.ProfileID)
			logger.Print("WX7", "已请求停止Undetectable Profile")
		}
		logger.Print("WX7", "资源清理完成")
	}()

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 5*time.Minute)
	defer cancelTimeout()

	// 打开微信视频号助手发表页（附着的页面已在发表页时跳过导航）。
	// 不使用 chromedp.Navigate：其内部(responseAction)阻塞等待页面 load 事件，
	// JS 弹窗/长连接场景下 load 可能永不触发导致挂起；改为直接发起导航 + 轮询 URL 确认。
	var curURL string
	curCtx, cancelCur := context.WithTimeout(tabCtx, 5*time.Second)
	_ = chromedp.Run(curCtx, chromedp.Location(&curURL))
	cancelCur()
	if strings.Contains(curURL, "channels.weixin.qq.com/platform/post/create") {
		logger.Print("WX2", "当前页面已在发表页，跳过导航: "+curURL)
	} else {
		logger.Print("WX2", "开始导航到发表页")
		navCtx, cancelNav := context.WithTimeout(tabCtx, 15*time.Second)
		err = chromedp.Run(navCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, errorText, navErr := page.Navigate("https://channels.weixin.qq.com/platform/post/create").Do(ctx)
			if navErr != nil {
				return navErr
			}
			if errorText != "" {
				return errors.New("page load error " + errorText)
			}
			return nil
		}))
		cancelNav()
		if err != nil {
			logger.Print("WX2", "发起导航失败: "+err.Error())
			logCancelDiagnostics(httpBase, tabCtx, allocCtx, ctx, logger, "发起导航失败")
		}
		// 轮询确认导航生效（不依赖 load 事件；若被 JS 弹窗阻塞，弹窗处理器会自动接受后恢复）
		navOK := false
		pollDeadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(pollDeadline) {
			var loc string
			locCtx, cancelLoc := context.WithTimeout(tabCtx, 3*time.Second)
			locErr := chromedp.Run(locCtx, chromedp.Location(&loc))
			cancelLoc()
			if locErr == nil && strings.Contains(loc, "channels.weixin.qq.com") {
				navOK = true
				logger.Print("WX2", "导航已生效: "+loc)
				break
			}
			time.Sleep(1 * time.Second)
		}
		if !navOK {
			logger.Print("WX2", "30秒内未确认导航生效(可能被JS弹窗阻塞或标签页异常)，继续尝试推进")
		}
	}

	// 导航发起后再启用网络诊断：避免 network.Enable 与 Navigate 的加载事件等待产生交互导致挂起；
	// 子应用资源由页面 JS 在挂载阶段拉取，仍能捕获加载失败。
	enableNetworkDiagnostics(tabCtx, logger)

	// 激活标签页：部分页面的初始化/渲染依赖标签页处于前台激活状态，远程附着时标签页可能在后台
	afCtx, cancelAf := context.WithTimeout(tabCtx, 5*time.Second)
	if err := chromedp.Run(afCtx, page.BringToFront()); err != nil {
		logger.Print("WX2", "激活标签页失败: "+err.Error())
	}
	cancelAf()

	// 显式设置窗口尺寸并置为普通态：wujie 子应用挂载与上传区渲染依赖容器有真实宽高，
	// CDP 新建/附着的标签页窗口可能尺寸异常(0或极小)导致子应用不渲染。
	szCtx, cancelSz := context.WithTimeout(tabCtx, 5*time.Second)
	if err := chromedp.Run(szCtx, chromedp.ActionFunc(func(c context.Context) error {
		windowID, _, e := browser.GetWindowForTarget().Do(c)
		if e != nil {
			return e
		}
		bounds := &browser.Bounds{WindowState: browser.WindowStateNormal, Left: 0, Top: 0, Width: 1512, Height: 920}
		return browser.SetWindowBounds(windowID, bounds).Do(c)
	})); err != nil {
		logger.Print("WX2", "设置窗口尺寸失败(不影响主流程): "+err.Error())
	} else {
		logger.Print("WX2", "已设置窗口尺寸为 1512x920")
	}
	cancelSz()

	// 等待页面重定向稳定：未登录时视频号助手会从 /platform/post/create 跳到 /login.html 扫码页。
	// 实测登录态失效时：URL 检测时刻地址栏仍是 post/create，login.html 稍后才加载，
	// 甚至存在 URL 不变、原页内直接嵌入 qrconnect 扫码 iframe 的形态——单次 URL 检查必然漏判，需持续观察。
	if needLogin := waitLoginRedirect(tabCtx, logger); needLogin {
		return errors.New("当前为视频号登录页面，需要扫码才能继续后续操作")
	}

	// 前置检查账号异常/功能不可用（仅匹配账号受限类专属文案，避免命中发表页的合规提示）
	if kw := checkAnomalyText(frameScope{ctx: tabCtx}); kw != "" {
		return fmt.Errorf("WX2 weixin channels account anomaly pre-check (matched '%s'): account restricted", kw)
	}

	// 输出主框架页面快照：确认发表页应用是否真正挂载渲染（登录判定可能放行未渲染的空白壳页面）
	dumpMainSnapshot(tabCtx, logger)

	// 定位发表页主体内容所在的 iframe（上传控件/描述输入框/发表按钮都在 iframe 内，主框架查不到）。
	// 仅记录 src 地址，后续每轮操作按 src 重新解析节点，避免页面重载后节点失效。
	iframeSrc := resolveContentFrame(tabCtx, logger)
	mainFS := frameScope{ctx: tabCtx, logger: logger}
	iframeFS := frameScope{ctx: tabCtx, logger: logger, iframeSrc: iframeSrc}
	if iframeSrc != "" {
		logger.Print("WX2", "已定位内容iframe，后续页面操作在iframe内执行")
	}

	// 上传视频文件（主框架与 iframe 双作用域轮询）
	if err := waitAndUploadFile(mainFS, iframeFS, logger, absVideoPath); err != nil {
		return fmt.Errorf("WX3 %v", err)
	}

	// 关闭上传后的提示弹窗（主框架与 iframe 都尝试，缩短超时，快速退出）
	_ = dismissPopups(mainFS, logger)
	_ = dismissPopups(iframeFS, logger)

	// 智能等待视频上传完成（进度文本消失 + 封面/发表按钮出现）
	if err := waitForUploadComplete(iframeFS, logger); err != nil {
		return fmt.Errorf("WX4 %v", err)
	}

	// 上传完成后可能弹出引导弹窗，需再次关闭（否则遮挡文案输入框和发表按钮）
	_ = dismissPopups(mainFS, logger)
	_ = dismissPopups(iframeFS, logger)

	// 填写视频描述/文案（内容在 iframe 内，解析失败自动回退主框架）
	if req.Text != "" {
		if err := fillText(iframeFS, logger, req.Text); err != nil {
			return fmt.Errorf("WX5 %v", err)
		}
	}
	logger.Print("WX6", "已填写描述，等待点击发表")

	// 短暂等待确保视频处理完成后再点击发表（封面已出现时无需长等待）
	time.Sleep(5 * time.Second)

	if err := clickPublish(iframeFS, logger); err != nil {
		return fmt.Errorf("WX6 %v", err)
	}

	// 等待页面跳转确认发布成功
	redirectOK := false
	verificationHandled := false
	for attempt := 1; attempt <= 2; attempt++ {
		logger.Print("WX6", fmt.Sprintf("已点击发表，等待页面跳转 (第%d次)", attempt))
		redirectDeadline := time.Now().Add(120 * time.Second)
		redirectOK = false
		for time.Now().Before(redirectDeadline) {
			// 流程中途被踢回登录页（登录态失效）：立即报错，不再傻等跳转/重试点击发表（重复点击会重复发布）
			var locNow string
			locChkCtx, cancelLocChk := context.WithTimeout(tabCtx, 1500*time.Millisecond)
			_ = chromedp.Run(locChkCtx, chromedp.Location(&locNow))
			cancelLocChk()
			if strings.Contains(locNow, "login.html") {
				return errors.New("当前为视频号登录页面，需要扫码才能继续后续操作")
			}
			// 网络层 post_create 已受理 = 发布成功（指纹浏览器下 wujie 子应用 URL 可能不变，不能仅凭 URL 判转）
			if atomic.LoadInt32(&publishNetSignal) == 1 {
				logger.Print("WX6", "网络层检测到 post_create 成功请求，判定发布成功")
				redirectOK = true
				break
			}
			// 检测是否出现验证码弹窗（限定弹窗容器内，避免误匹配页面普通文案）
			if !verificationHandled {
				if verifyModalVisible(tabCtx) {
					logger.Print("WX6", "⚠️ 检测到验证码弹窗，请在浏览器中手动输入验证码并完成验证")
					logger.Print("WX6", "等待人工处理验证码... (最长等待3分钟)")
					verificationHandled = true
					// 等待验证码弹窗消失(用户手动处理)；发布信号出现则立即退出等待（验证已自动通过）
					verifyDeadline := time.Now().Add(3 * time.Minute)
					for time.Now().Before(verifyDeadline) {
						time.Sleep(3 * time.Second)
						if atomic.LoadInt32(&publishNetSignal) == 1 {
							logger.Print("WX6", "验证期间检测到 post_create 成功请求，判定发布成功")
							break
						}
						if !verifyModalVisible(tabCtx) {
							logger.Print("WX6", "验证码弹窗已消失，继续等待页面跳转")
							break
						}
					}
					if atomic.LoadInt32(&publishNetSignal) == 1 {
						redirectOK = true
						break
					}
				}
			}

			var href string
			locCtx, cancelLoc := context.WithTimeout(tabCtx, 1500*time.Millisecond)
			_ = chromedp.Run(locCtx, chromedp.Location(&href))
			cancelLoc()
			if strings.Contains(href, "post/list") || strings.Contains(href, "post/manage") {
				redirectOK = true
				break
			}
			time.Sleep(800 * time.Millisecond)
		}
		if redirectOK {
			break
		}
		if attempt < 2 {
			logger.Print("WX6", "超时未跳转，重试点击发表")
			if err := clickPublish(iframeFS, logger); err != nil {
				logger.Print("WX6", "重试点击发表失败: "+err.Error())
				break
			}
		} else {
			// 第二轮仍未跳转：输出页面诊断，定位发表卡点（弹窗遮挡/报错/按钮未响应）
			logger.Print("WX6", "两轮点击后仍未跳转，页面诊断: "+uploadDiagnostics(iframeFS))
		}
	}
	if !redirectOK {
		// 兜底：检查页面是否出现发布成功的提示
		successCheckCtx, cancelSuccess := context.WithTimeout(tabCtx, 3*time.Second)
		var successNodes []*cdp.Node
		_ = chromedp.Run(successCheckCtx, chromedp.Nodes(`//*[contains(text(), '发表成功') or contains(text(), '已发表')]`, &successNodes, chromedp.BySearch))
		cancelSuccess()
		if len(successNodes) > 0 {
			logger.Print("WX6", "检测到发表成功提示，判定发布成功")
			redirectOK = true
		}
	}
	if !redirectOK {
		logger.Print("WX6", "重试后仍未跳转，未知原因需要人为检查")
		time.Sleep(8 * time.Second)
		return errors.New("WX6 未跳转至视频号内容管理页，未知原因需要人为检查")
	}
	logger.Print("WX6", "发布成功")
	time.Sleep(8 * time.Second)
	if err := os.Remove(absVideoPath); err != nil {
		logger.Print("WX8", "删除本地视频失败: "+err.Error())
	} else {
		logger.Print("WX8", "已删除本地视频: "+absVideoPath)
	}
	return nil
}

// verifyModalVisible 检测页面是否出现验证码类弹窗：含验证文案的可见元素且位于 modal/dialog 容器内。
// 旧版全文档 XPath 搜"验证码"会误匹配页面普通文案，导致发布成功后仍傻等人工验证。
func verifyModalVisible(tabCtx context.Context) bool {
	var visible bool
	js := `(function(){
		var all = document.querySelectorAll('div, section, p, span, h1, h2, h3, h4, label');
		for (var i = 0; i < all.length; i++) {
			var el = all[i];
			var t = ((el.innerText || '') + '').trim();
			if (t.length > 60) continue;
			if (t.indexOf('验证码') < 0 && t.indexOf('安全验证') < 0 && t.indexOf('手机验证') < 0) continue;
			var r = el.getBoundingClientRect();
			if (r.width <= 0 || r.height <= 0) continue;
			var p = el;
			for (var k = 0; k < 10 && p; k++) {
				var cls = ((p.className || '') + '').toLowerCase();
				if (cls.indexOf('modal') >= 0 || cls.indexOf('dialog') >= 0 || cls.indexOf('popup') >= 0 || cls.indexOf('mask') >= 0) return true;
				p = p.parentElement;
			}
		}
		return false;
	})()`
	vCtx, cancel := context.WithTimeout(tabCtx, 3*time.Second)
	defer cancel()
	_ = chromedp.Run(vCtx, chromedp.Evaluate(js, &visible))
	return visible
}

// waitLoginRedirect 持续观察登录态失效信号，返回 true 表示停在登录页需要扫码。
// 失效形态有三种，单次 URL 检查只能命中第一种：
// 1) 跳转到 /login.html（稍晚于导航检测时刻）；2) URL 不变，原页内直接嵌入
// open.weixin.qq.com/connect/qrconnect 扫码 iframe；3) URL 含 login 的其他变体。
// 反向信号：页面出现 file input（发表页主体已渲染）立即判已登录，避免无谓等待。
func waitLoginRedirect(tabCtx context.Context, logger *logx.Logger) bool {
	deadline := time.Now().Add(18 * time.Second)
	for time.Now().Before(deadline) {
		var href string
		locCtx, cancelLoc := context.WithTimeout(tabCtx, 2*time.Second)
		_ = chromedp.Run(locCtx, chromedp.Location(&href))
		cancelLoc()
		if strings.Contains(href, "login") {
			logger.Print("WX2", "检测到登录页URL: "+href)
			return true
		}
		// 页面特征：嵌入了扫码组件 iframe（qrconnect），或出现可见的扫码文案且无发表页主体。
		// 用 JS 一次判完，比 CDP 节点搜索更快且能精确限定容器。
		var state string
		jsCtx, cancelJS := context.WithTimeout(tabCtx, 3*time.Second)
		_ = chromedp.Run(jsCtx, chromedp.Evaluate(`(function(){
			var fs = document.querySelectorAll('input[type="file"]');
			for (var i = 0; i < fs.length; i++) { return 'loggedin'; }
			var ifs = document.querySelectorAll('iframe');
			for (var j = 0; j < ifs.length; j++) {
				var src = ifs[j].src || '';
				if (src.indexOf('qrconnect') >= 0 || src.indexOf('snsapi_login') >= 0) return 'login-iframe';
			}
			return '';
		})()`, &state))
		cancelJS()
		if state == "loggedin" {
			logger.Print("WX2", "页面已出现发表页文件输入框，判定已登录")
			return false
		}
		if state == "login-iframe" {
			logger.Print("WX2", "页面嵌入了微信扫码组件(qrconnect)，判定为登录态失效")
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// httpBaseFromWS 从 WebSocket 调试地址推导 HTTP 调试接口地址：
// ws://127.0.0.1:9222/devtools/browser/xxx → http://127.0.0.1:9222/
func httpBaseFromWS(wsURL string) string {
	u := strings.TrimPrefix(wsURL, "ws://")
	u = strings.TrimPrefix(u, "wss://")
	if i := strings.Index(u, "/"); i > 0 {
		u = u[:i]
	}
	return "http://" + u + "/"
}

// findWeixinTab 通过 HTTP 调试接口查找已打开的微信视频号平台 page 标签，返回其 id 与调试 ws 地址（无则均为空）。
// 不依赖 CDP 会话，会话异常时也能可靠获取标签页状态。
func findWeixinTab(httpBase string, logger *logx.Logger) struct{ ID, WSURL string } {
	var none struct{ ID, WSURL string }
	resp, err := http.Get(httpBase + "json/list")
	if err != nil {
		logger.Print("WX1", "获取标签页列表(HTTP)失败: "+err.Error())
		return none
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return none
	}
	var tabs []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		URL   string `json:"url"`
		WSURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &tabs); err != nil {
		return none
	}
	for _, t := range tabs {
		if t.Type == "page" && strings.Contains(t.URL, "channels.weixin.qq.com/platform") {
			return struct{ ID, WSURL string }{t.ID, t.WSURL}
		}
	}
	return none
}

// pickReusableTab 从浏览器现有标签中挑一个可复用的普通标签页（直连其调试 ws）。
// 实测该指纹浏览器会销毁自动化新建的标签页，复用现有标签导航是唯一可靠的打开方式。
// 跳过：微信页（应由 findWeixinTab 处理）、浏览器内部页、扩展页、可疑的 http://data/ 页。
func pickReusableTab(httpBase string, logger *logx.Logger) struct{ ID, WSURL string } {
	var none struct{ ID, WSURL string }
	resp, err := http.Get(httpBase + "json/list")
	if err != nil {
		return none
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return none
	}
	var tabs []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		URL   string `json:"url"`
		WSURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &tabs); err != nil {
		return none
	}
	for _, t := range tabs {
		if t.Type != "page" || t.WSURL == "" {
			continue
		}
		if strings.Contains(t.URL, "channels.weixin.qq.com") ||
			strings.HasPrefix(t.URL, "chrome") ||
			strings.HasPrefix(t.URL, "about:") ||
			strings.HasPrefix(t.URL, "http://data") {
			continue
		}
		logger.Print("WX1", "选中可复用标签页: "+t.URL)
		return struct{ ID, WSURL string }{t.ID, t.WSURL}
	}
	return none
}

// closeTabViaHTTP 通过 HTTP 调试接口关闭指定标签页（不依赖 CDP 会话），返回是否执行了关闭请求。
func closeTabViaHTTP(httpBase, tabID string, logger *logx.Logger) bool {
	if tabID == "" {
		return false
	}
	resp, err := http.Get(httpBase + "json/close/" + tabID)
	if err != nil {
		logger.Print("WX7", "HTTP关闭标签页失败: "+err.Error())
		return false
	}
	resp.Body.Close()
	logger.Print("WX7", "已通过HTTP关闭标签页: "+tabID)
	return true
}

// logCancelDiagnostics 会话异常时输出诊断：区分是哪一级上下文被取消，并重新检查微信标签页是否仍存在。
// 标签页消失 → 标签被浏览器关闭；标签仍在但连接级取消 → 浏览器级调试连接断开（指纹浏览器常见）。
func logCancelDiagnostics(httpBase string, tabCtx, allocCtx, outerCtx context.Context, logger *logx.Logger, note string) {
	detail := ""
	switch {
	case outerCtx.Err() != nil:
		detail = "外层ctx已取消(" + outerCtx.Err().Error() + ")"
	case allocCtx.Err() != nil:
		detail = "浏览器连接级ctx已取消(" + allocCtx.Err().Error() + ")，多为浏览器级调试ws断连"
	case tabCtx.Err() != nil:
		detail = "标签页级ctx已取消(" + tabCtx.Err().Error() + ")"
	default:
		detail = "各级ctx均未取消（可能是单次命令超时）"
	}
	if findWeixinTab(httpBase, logger).ID == "" {
		detail += "；微信标签页已不存在(被浏览器关闭)"
	} else {
		detail += "；微信标签页仍存在"
	}
	logger.Print("WXERR", note+" → "+detail)
}

// dumpMainSnapshot 输出主框架页面快照（标题/正文文本预览/关键容器/iframe原始HTML），
// 用于确认发表页应用是否真正挂载渲染，以及 empty.html iframe 是否带 sandbox 等属性。
func dumpMainSnapshot(ctx context.Context, logger *logx.Logger) {
	var info string
	snapCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := chromedp.Run(snapCtx, chromedp.Evaluate(`(function(){
		var out = [];
		out.push('title=' + document.title);
		out.push('body子元素数=' + (document.body ? document.body.children.length : -1));
		out.push('container-wrap存在=' + !!document.querySelector('#container-wrap'));
		var t = document.body ? (document.body.innerText || '') : '';
		out.push('body文本长度=' + t.length);
		out.push('body文本预览=' + JSON.stringify(t.slice(0, 300)));
		var ifr = document.querySelector('iframe');
		if (ifr) {
			out.push('iframe[0]outerHTML=' + ifr.outerHTML.slice(0, 300));
		}
		return out.join('\n');
	})()`, &info))
	if err != nil {
		logger.Print("WX2", "页面快照获取失败: "+err.Error())
		return
	}
	logger.Print("WX2", "页面快照:\n"+info)
}

// registerConsoleDiagnostics 注册控制台错误/JS异常监听。
// 发表区由 wujie 微前端子应用 JS 渲染，子应用挂载失败时的报错是定位渲染不出来的关键线索。
// 必须在 Navigate 之前注册（调用前需已有任意一次 chromedp.Run 以初始化 target 会话）。
// 注意：此函数只启用 Runtime 域；Network 域在导航发起后再启用（实测提前启用会导致 Navigate 挂起）。
func registerConsoleDiagnostics(tabCtx context.Context, logger *logx.Logger) {
	enableCtx, cancelEnable := context.WithTimeout(tabCtx, 5*time.Second)
	defer cancelEnable()
	if err := chromedp.Run(enableCtx, runtime.Enable()); err != nil {
		logger.Print("WX2", "启用调试事件失败(不影响主流程): "+err.Error())
		return
	}
	chromedp.ListenTarget(tabCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			if e.Type != "error" {
				return
			}
			parts := make([]string, 0, len(e.Args))
			for _, a := range e.Args {
				if a == nil {
					continue
				}
				if a.Value != nil {
					parts = append(parts, string(a.Value))
				} else if a.Description != "" {
					parts = append(parts, a.Description)
				}
			}
			msg := strings.Join(parts, " ")
			if msg == "" {
				return
			}
			if len(msg) > 300 {
				msg = msg[:300]
			}
			logger.Print("WXDBG", "console.error: "+msg)
		case *runtime.EventExceptionThrown:
			if e.ExceptionDetails == nil {
				return
			}
			msg := e.ExceptionDetails.Text
			if e.ExceptionDetails.Exception != nil && e.ExceptionDetails.Exception.Description != "" {
				msg = e.ExceptionDetails.Exception.Description
			}
			if len(msg) > 300 {
				msg = msg[:300]
			}
			logger.Print("WXDBG", "JS异常: "+msg)
		}
	})
	logger.Print("WX2", "已注册控制台/异常诊断监听")
}

// setupDialogHandler 监听并自动接受页面 JS 弹窗（alert/confirm/beforeunload），同时打印弹窗内容。
// JS 弹窗会阻塞整个渲染器的 JS 执行（load 事件不触发、所有 Runtime 命令挂起），必须在导航前注册。
// 注意：不能在监听回调里直接调用 chromedp.Run（事件派发循环内会死锁），必须开 goroutine 处理。
func setupDialogHandler(tabCtx context.Context, logger *logx.Logger) {
	chromedp.ListenTarget(tabCtx, func(ev interface{}) {
		e, ok := ev.(*page.EventJavascriptDialogOpening)
		if !ok {
			return
		}
		msg := e.Message
		if len(msg) > 200 {
			msg = msg[:200]
		}
		logger.Print("WXDBG", fmt.Sprintf("检测到JS弹窗 type=%s message=%s，自动接受", e.Type, msg))
		go func() {
			hCtx, cancel := context.WithTimeout(tabCtx, 5*time.Second)
			defer cancel()
			if err := chromedp.Run(hCtx, page.HandleJavaScriptDialog(true)); err != nil {
				logger.Print("WXDBG", "自动接受JS弹窗失败: "+err.Error())
			}
		}()
	})
	logger.Print("WX2", "已注册JS弹窗自动处理")
}

// enableNetworkDiagnostics 启用网络失败诊断（在导航发起后调用）：
// 追踪请求并在加载失败时打印资源 URL，用于定位子应用资源加载失败。
func enableNetworkDiagnostics(tabCtx context.Context, logger *logx.Logger) {
	enableCtx, cancelEnable := context.WithTimeout(tabCtx, 5*time.Second)
	defer cancelEnable()
	if err := chromedp.Run(enableCtx, network.Enable()); err != nil {
		logger.Print("WX2", "启用网络诊断失败(不影响主流程): "+err.Error())
		return
	}
	var netMu sync.Mutex
	reqURLs := make(map[network.RequestID]string)
	chromedp.ListenTarget(tabCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			if e.Request.URL != "" {
				// 上传分片请求出现 = 前端已消费文件并启动上传（比页面文案更早更可靠）
				if isUploadNetURL(e.Request.URL) {
					atomic.StoreInt32(&uploadNetSignal, 1)
				}
				netMu.Lock()
				reqURLs[e.RequestID] = e.Request.URL
				netMu.Unlock()
			}
		case *network.EventResponseReceived:
			// 全量响应日志(诊断用)：定位子应用入口/资源是否被请求及其状态码。
			netMu.Lock()
			u := reqURLs[e.RequestID]
			netMu.Unlock()
			if u == "" {
				u = e.Response.URL
			}
			if isUploadNetURL(u) {
				atomic.StoreInt32(&uploadNetSignal, 1)
			}
			// post_create 请求发出 = 服务端已受理发表（比页面跳转更早更可靠）
			if isPublishNetURL(u) && e.Response.Status >= 200 && e.Response.Status < 400 {
				atomic.StoreInt32(&publishNetSignal, 1)
			}
			if len(u) > 140 {
				u = u[:140]
			}
			logger.Print("WXNET", fmt.Sprintf("[%d] %s", e.Response.Status, u))
		case *network.EventLoadingFailed:
			netMu.Lock()
			u := reqURLs[e.RequestID]
			delete(reqURLs, e.RequestID)
			netMu.Unlock()
			if len(u) > 120 {
				u = u[:120]
			}
			logger.Print("WXDBG", "资源加载失败: "+u+" err="+e.ErrorText)
		}
	})
	logger.Print("WX2", "已启用网络失败诊断")
}

// dumpRenderStructure 列出主框架容器子树结构，辅助判断 wujie 子应用是否挂载（#container-wrap 是否有子内容、是否存在自定义元素容器）
func dumpRenderStructure(fs frameScope, logger *logx.Logger) {
	var info string
	js := `(function(){
		var out = [];
		var wrap = document.querySelector('#container-wrap');
		if (wrap) {
			out.push('container-wrap子元素数=' + wrap.children.length);
			for (var i = 0; i < wrap.children.length && i < 8; i++) {
				var c = wrap.children[i];
				out.push('  wrap子[' + i + '] tag=' + c.tagName + ' class=' + (('' + (c.className || '')).slice(0, 60)) + ' children=' + c.children.length + ' text=' + JSON.stringify(('' + (c.innerText || '')).slice(0, 50)));
			}
		} else {
			out.push('container-wrap不存在');
		}
		var customs = document.querySelectorAll('*');
		var names = {};
		for (var j = 0; j < customs.length; j++) {
			var t = customs[j].tagName;
			if (t.indexOf('-') > 0) names[t] = (names[t] || 0) + 1;
		}
		out.push('自定义元素(含shadow容器候选): ' + JSON.stringify(names));
		return out.join('\n');
	})()`
	if err := fs.evaluate(&info, js, 5*time.Second); err != nil {
		logger.Print("WX3", "渲染结构诊断失败: "+err.Error())
		return
	}
	logger.Print("WX3", "渲染结构:\n"+info)
}

// dumpWujieState 深度诊断 wujie 微前端子应用的挂载状态：
// 窗口/容器实际尺寸、wujie-app 元素属性与 shadow DOM 子节点数、wujie 沙箱 iframe 的 src/可见性，
// 以及 window 上是否存在 wujie 运行时对象。用于判定子应用是"未开始加载"还是"加载了但渲染失败"。
func dumpWujieState(fs frameScope, logger *logx.Logger) {
	var info string
	js := `(function(){
		var out = [];
		out.push('window尺寸=' + window.innerWidth + 'x' + window.innerHeight + ' dpr=' + (window.devicePixelRatio||1));
		out.push('document.visibilityState=' + document.visibilityState);
		var cc = document.querySelector('.container-center') || document.querySelector('#container-wrap');
		if (cc) {
			out.push('内容容器尺寸=' + cc.offsetWidth + 'x' + cc.offsetHeight);
		}
		var apps = document.querySelectorAll('wujie-app');
		out.push('wujie-app数量=' + apps.length);
		for (var i = 0; i < apps.length && i < 3; i++) {
			var a = apps[i];
			var attrs = {};
			for (var k = 0; k < a.attributes.length; k++) attrs[a.attributes[k].name] = (''+a.attributes[k].value).slice(0,80);
			out.push('wujie-app[' + i + ']属性=' + JSON.stringify(attrs));
			var childCount = a.children ? a.children.length : -1;
			var shadowCount = -1;
			try { shadowCount = a.shadowRoot ? a.shadowRoot.childNodes.length : -2; } catch(e) {}
			out.push('wujie-app[' + i + '] lightChildren=' + childCount + ' shadowChildren=' + shadowCount);
			if (a.shadowRoot) {
				var txt = (a.shadowRoot.textContent || '').replace(/\s+/g,' ').slice(0,100);
				out.push('wujie-app[' + i + ']shadowText=' + JSON.stringify(txt));
			}
		}
		var ifrs = document.querySelectorAll('iframe');
		for (var f = 0; f < ifrs.length && f < 4; f++) {
			var ifr = ifrs[f];
			var st = window.getComputedStyle(ifr);
			out.push('iframe[' + f + '] name=' + (ifr.name||'') + ' display=' + st.display + ' 尺寸=' + ifr.offsetWidth + 'x' + ifr.offsetHeight + ' src=' + (ifr.src||'(无)').slice(0,90));
		}
		out.push('window.__WUJIE存在=' + (typeof window.__WUJIE !== 'undefined'));
		out.push('window.__POWERED_BY_WUJIE__=' + (typeof window.__POWERED_BY_WUJIE__ !== 'undefined'));
		if (window.__WXERRLOG && window.__WXERRLOG.length) {
			out.push('页面错误收集(' + window.__WXERRLOG.length + '条):');
			for (var i = 0; i < window.__WXERRLOG.length && i < 20; i++) {
				out.push('  #' + i + ' ' + window.__WXERRLOG[i]);
			}
		} else {
			out.push('页面错误收集: 无记录');
		}
		return out.join('\n');
	})()`
	if err := fs.evaluate(&info, js, 5*time.Second); err != nil {
		logger.Print("WX3", "wujie状态诊断失败: "+err.Error())
		return
	}
	logger.Print("WX3", "wujie子应用状态:\n"+info)
}

// dumpCrossFrameOverview 跨框架全景诊断：递归遍历主文档、同源 iframe 的 contentDocument、
// 所有 shadow roots，逐个输出 URL/文本长度/input数/含"上传"文案元素数，
// 用于确认 wujie 子应用到底渲染在哪个可达文档里（主框架 CDP 查询够不到沙箱 iframe，实测教训）。
func dumpCrossFrameOverview(fs frameScope, logger *logx.Logger) {
	js := `(function(){
		var out = [];
		var seen = [];
		function describe(r, label){
			try {
				var url = '';
				try { url = (r.location && r.location.href) || ''; } catch(e){}
				var txtLen = 0, bodyKids = -1;
				try { var b = r.body || r.querySelector && r.querySelector('body'); txtLen = b ? ((b.innerText||'').length) : 0; bodyKids = b ? b.children.length : -1; } catch(e2){}
				var inputs = -1, uploads = -1, btns = -1;
				try { inputs = r.querySelectorAll('input').length; uploads = r.querySelectorAll('input[type="file"]').length; btns = r.querySelectorAll('button').length; } catch(e3){}
				var sample = '';
				try { var bb = r.body; sample = bb ? ((bb.innerText||'').replace(/\s+/g,' ').slice(0,80)) : ''; } catch(e4){}
				out.push(label + ' url=' + (url||'(无)').slice(0,70) + ' body子=' + bodyKids + ' 文本长=' + txtLen + ' input=' + inputs + ' fileInput=' + uploads + ' button=' + btns + ' 文本=' + JSON.stringify(sample));
			} catch(e5){ out.push(label + ' 读取失败'); }
		}
		function walk(r, depth, prefix){
			if (depth > 4 || seen.indexOf(r) >= 0 || seen.length > 30) return;
			seen.push(r);
			describe(r, prefix);
			try {
				var ifrs = r.querySelectorAll('iframe');
				for (var i = 0; i < ifrs.length; i++) {
					try { var cd = ifrs[i].contentDocument; if (cd) walk(cd, depth+1, prefix + '>iframe[' + (ifrs[i].name||i) + ']'); else out.push(prefix + '>iframe[' + (ifrs[i].name||i) + '] 跨域/未加载'); } catch(e){}
				}
				var els = r.querySelectorAll('*');
				for (var j = 0; j < els.length; j++) {
					var sr = els[j].shadowRoot;
					if (sr) walk(sr, depth+1, prefix + '>shadow(' + (els[j].tagName||'?').toLowerCase() + ')');
				}
			} catch(e6){}
		}
		walk(document, 0, 'root');
		return out.join('\n');
	})()`
	var info string
	evalCtx, cancel := context.WithTimeout(fs.ctx, 8*time.Second)
	defer cancel()
	if err := chromedp.Run(evalCtx, chromedp.Evaluate(js, &info)); err != nil {
		logger.Print("WX3", "跨框架全景诊断失败: "+err.Error())
		return
	}
	logger.Print("WX3", "跨框架可达文档:\n"+info)
}

// errCollectorScript 导航前注入的全局错误收集脚本（在任何页面 JS 之前执行）。
// 视频号助手主应用用 console.error(event) 记录错误，裸 Event 序列化后只剩 "Event"，
// 这里把 Event/Error/普通对象的关键字段展开成可读文本，存入 window.__WXERRLOG 供诊断打印。
const errCollectorScript = `(function(){
	window.__WXERRLOG = window.__WXERRLOG || [];
	function push(s){ try { if (window.__WXERRLOG.length < 200) window.__WXERRLOG.push((''+s).slice(0,400)); } catch(e){} }
	function fmtArg(a){
		try {
			if (a === null) return 'null';
			if (a === undefined) return 'undefined';
			var t = typeof a;
			if (t === 'string') return a;
			if (t === 'number' || t === 'boolean') return String(a);
			if (a instanceof Error) return (a.name||'Error') + ': ' + (a.message||'') + ' @' + ((a.stack||'').split('\n')[1]||'').trim();
			if (a instanceof Event) {
				var tgt = '';
				try { if (a.target && a.target.src) tgt = ' target.src=' + a.target.src; else if (a.target && a.target.tagName) tgt = ' target=' + a.target.tagName; } catch(e2){}
				return 'Event{type=' + a.type + tgt + (a.message ? ' message=' + a.message : '') + (a.filename ? ' file=' + a.filename : '') + (a.error ? ' error=' + String(a.error.message || a.error) : '') + '}';
			}
			if (t === 'object') { var j = ''; try { j = JSON.stringify(a); } catch(e3){ j = Object.prototype.toString.call(a); } return (j||'').slice(0,200); }
			return String(a);
		} catch(e){ return '[unfmt]'; }
	}
	window.addEventListener('error', function(e){ push('[win.error] ' + fmtArg(e)); }, true);
	window.addEventListener('unhandledrejection', function(e){ push('[unhandledrejection] ' + fmtArg(e && e.reason)); }, true);
	var origErr = console.error;
	console.error = function(){
		try { var parts = []; for (var i = 0; i < arguments.length; i++) parts.push(fmtArg(arguments[i])); push('[console.error] ' + parts.join(' ')); } catch(e){}
		return origErr.apply(console, arguments);
	};
})();`

// resolveContentFrame 定位发表页主体内容所在的 iframe，返回其 src（未找到返回空串）。
// 视频号助手的上传控件/描述输入框/发表按钮渲染在 iframe 内，主框架上查不到；
// 该 iframe 初始为 empty.html，内容稍后才会加载，因此只取 src，节点在每次操作时重新解析。
func resolveContentFrame(ctx context.Context, logger *logx.Logger) string {
	// 先列出所有 iframe 的 src，便于排查
	var srcs []string
	listCtx, cancelList := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(listCtx, chromedp.Evaluate(`(function(){
		var fs = document.querySelectorAll('iframe');
		var out = [];
		for (var i = 0; i < fs.length; i++) out.push(fs[i].src || '(无src)');
		return out;
	})()`, &srcs))
	cancelList()
	logger.Print("WX2", fmt.Sprintf("检测到 %d 个iframe: %v", len(srcs), srcs))
	for _, s := range srcs {
		if s != "" && s != "(无src)" {
			return s
		}
	}
	return ""
}

// frameScope 页面操作的作用域：设置 iframeSrc 时优先在对应 iframe 内操作（节点每次按 src 重新解析，
// 避免页面重载后失效）；未设置或解析失败时回退主框架。
type frameScope struct {
	ctx       context.Context
	logger    *logx.Logger
	iframeSrc string    // 目标 iframe 的 src；为空表示主框架作用域
	iframe    *cdp.Node // 最近一次解析到的节点缓存（仅供 queryOpts 复用）
}

// resolveNode 按 src 重新解析 iframe 节点。失败时清空缓存并返回错误，由调用方决定回退主框架。
func (fs *frameScope) resolveNode() (*cdp.Node, error) {
	if fs.iframeSrc == "" {
		return nil, errors.New("frameScope: 主框架作用域")
	}
	sel := fmt.Sprintf(`iframe[src=%q]`, fs.iframeSrc)
	resCtx, cancel := context.WithTimeout(fs.ctx, 3*time.Second)
	defer cancel()
	var nodes []*cdp.Node
	if err := chromedp.Run(resCtx, chromedp.Nodes(sel, &nodes, chromedp.ByQuery)); err != nil || len(nodes) == 0 {
		// 兼容 src 变化：退化为取第一个 iframe
		var fallback []*cdp.Node
		fbCtx, cancelFb := context.WithTimeout(fs.ctx, 2*time.Second)
		_ = chromedp.Run(fbCtx, chromedp.Nodes(`iframe`, &fallback, chromedp.ByQuery))
		cancelFb()
		if len(fallback) > 0 {
			fs.iframe = fallback[0]
			return fallback[0], nil
		}
		fs.iframe = nil
		return nil, errors.New("frameScope: iframe节点未找到")
	}
	fs.iframe = nodes[0]
	return nodes[0], nil
}

// evalInFrame 在指定上下文中、于作用域框架内执行 JS 并把结果写入 res。
// iframe 作用域解析失败（节点失效/上下文已销毁）时回退主框架执行，不阻塞主流程。
// 注意：必须经 chromedp.Run 执行——它会根据节点 FrameID 把命令分发到对应框架的会话；
// 直接 .Do(ctx) 会走主会话，对 iframe 的 CreateIsolatedWorld/Execute 报 invalid context（实测教训）。
func (fs frameScope) evalInFrame(ctx context.Context, res interface{}, expression string) error {
	if fs.iframeSrc == "" {
		return chromedp.Run(ctx, chromedp.Evaluate(expression, res))
	}
	mutable := &fs
	node, err := mutable.resolveNode()
	if err != nil || node == nil || node.FrameID == "" {
		if fs.logger != nil {
			detail := ""
			if err != nil {
				detail = err.Error()
			} else {
				detail = "iframe无FrameID(文档未加载)"
			}
			fs.logger.Print("WX3", "iframe作用域不可用，回退主框架执行: "+detail)
		}
		return chromedp.Run(ctx, chromedp.Evaluate(expression, res))
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		worldID, err := page.CreateIsolatedWorld(node.FrameID).Do(c)
		if err != nil {
			if fs.logger != nil {
				fs.logger.Print("WX3", "iframe执行上下文创建失败，回退主框架执行: "+err.Error())
			}
			return chromedp.Evaluate(expression, res).Do(c)
		}
		val, _, err := runtime.Evaluate(expression).WithContextID(worldID).WithReturnByValue(true).Do(c)
		if err != nil {
			return err
		}
		if val == nil || val.Value == nil {
			return nil
		}
		return json.Unmarshal(val.Value, res)
	}))
}

// evaluate 带超时的框架内 JS 执行，失败返回 error。
// iframe 作用域直接走跨框架版本：CDP 对 wujie 沙箱 iframe 的直连会话不可靠（实测反复 invalid context），
// 而子应用与主框架同源，在主框架中递归遍历同源 iframe 的 contentDocument 与 shadow roots 逐根执行更可靠。
func (fs frameScope) evaluate(res interface{}, expression string, timeout time.Duration) error {
	if fs.iframeSrc == "" {
		evalCtx, cancel := context.WithTimeout(fs.ctx, timeout)
		defer cancel()
		return fs.evalInFrame(evalCtx, res, expression)
	}
	return fs.evaluateCrossFrame(res, expression, timeout)
}

// evaluateCrossFrame 跨框架执行 JS：主框架 + 同源 iframe 文档 + 所有 shadow roots 逐根执行 body，
// 返回第一个非零结果（布尔 true/非空字符串/非零数字）。body 须为可求值表达式。
// 用字符串替换而非 fmt.Sprintf 注入：body 内常含 %%（如上传进度正则），Sprintf 会破坏它们。
func (fs frameScope) evaluateCrossFrame(res interface{}, body string, timeout time.Duration) error {
	wrapped := strings.Replace(crossFrameWrapJS, "%s", body, 1)
	cfCtx, cancel := context.WithTimeout(fs.ctx, timeout)
	defer cancel()
	return chromedp.Run(cfCtx, chromedp.Evaluate(wrapped, res))
}

// crossFrameWrapJS 跨框架执行包装器：%s 处为待逐根执行的 JS 表达式。
// 收集主文档、同源 iframe 的 contentDocument、所有 shadow roots（限深限量防重入），
// 每根以形参屏蔽 document/window，使 body 中的 document/window 指向当前遍历的根，返回第一个非零结果。
const crossFrameWrapJS = `(function(){
	var roots = [];
	function addRoot(r){ try { if (r && roots.indexOf(r) < 0 && roots.length < 40) roots.push(r); } catch(e){} }
	addRoot(document);
	function walk(r, depth){
		if (depth > 4) return;
		try {
			var ifrs = r.querySelectorAll('iframe');
			for (var i = 0; i < ifrs.length; i++) {
				try { var cd = ifrs[i].contentDocument; if (cd) { addRoot(cd); walk(cd, depth+1); } } catch(e){}
			}
			var els = r.querySelectorAll('*');
			for (var j = 0; j < els.length; j++) {
				var sr = els[j].shadowRoot;
				if (sr) { addRoot(sr); walk(sr, depth+1); }
			}
		} catch(e){}
	}
	walk(document, 0);
	for (var k = 0; k < roots.length; k++) {
		try {
			var __d = roots[k], __w = null;
			try { __w = __d.defaultView; } catch(e2){}
			var r = (function(document, window){ return (%s); })(__d, __w);
			if (r === true || (typeof r === 'string' && r !== '') || (typeof r === 'number' && r !== 0)) return r;
		} catch(e4){}
	}
	return false;
})()`

// findFileInputCrossFrame 跨框架（主文档+同源iframe+shadow roots）查找上传控件/入口并尝试触发文件选择框：
// 找到 input[type=file] 时：直接点击；若是隐藏代理 input（element-ui 类组件常见），
// 同时点击其可见的上传容器/含"上传"文案的祖先，由视觉区域的原生 click 触发选择框（拦截器填入文件）。
// 返回命中描述（未命中返回空串）。wujie 子应用渲染在沙箱 iframe 内，必须同源 JS 穿透。
func findFileInputCrossFrame(fs frameScope, logger *logx.Logger) string {
	js := `(function(){
		var els = document.querySelectorAll('input[type="file"]');
		if (els.length === 0) return '';
		var inp = els[0];
		var vis = inp.offsetWidth > 0 && inp.offsetHeight > 0;
		var info = 'input[accept=' + (inp.accept||'') + ' visible=' + vis + ']';
		try { inp.click(); } catch(e){}
		var p = inp.parentElement;
		for (var d = 0; d < 10 && p; d++) {
			var t = ((p.innerText || '') + '').trim();
			var cls = ((p.className || '') + '');
			if ((t.indexOf('上传') >= 0 && t.length < 60) || cls.indexOf('upload') >= 0) {
				try { p.click(); } catch(e2){}
				info += ' clickedContainer=' + (p.tagName||'') + '.' + cls.slice(0, 40) + ' text=' + JSON.stringify(t.slice(0, 20));
				break;
			}
			p = p.parentElement;
		}
		return info;
	})()`
	var r string
	_ = fs.evaluateCrossFrame(&r, js, 5*time.Second)
	return r
}

// injectFileCrossFrame 直接通过 CDP 把视频文件注入沙箱 iframe 内的 input[type=file]（antd 上传组件的隐藏代理 input）：
// JS 点击在 iframe display:none 时不弹文件选择框、且合成点击的用户激活窗口短暂，
// 而 dom.SetFileInputFiles 按 BackendNodeID 注入不依赖可见性/焦点，是更可靠的途径。
// 注入后手动派发 change 事件（CDP 注入不触发事件，antd 组件靠它感知文件）。
func injectFileCrossFrame(fs frameScope, logger *logx.Logger, absVideoPath string) bool {
	injectCtx, cancel := context.WithTimeout(fs.ctx, 15*time.Second)
	defer cancel()
	err := chromedp.Run(injectCtx, chromedp.ActionFunc(func(c context.Context) error {
		// 1. 在主框架中直接引用同源沙箱 iframe 内的 input 元素，取 RemoteObject 句柄（不按 returnByValue）
		var ro *runtime.RemoteObject
		expr := `(function(){
			var f = document.querySelector('iframe[name="content"]') || document.querySelector('iframe[data-wujie-flag]') || document.querySelector('iframe');
			if (!f || !f.contentDocument) return null;
			return f.contentDocument.querySelector('input[type="file"]');
		})()`
		if err := chromedp.Evaluate(expr, &ro).Do(c); err != nil {
			return err
		}
		if ro == nil || ro.ObjectID == "" {
			return errors.New("未能在沙箱iframe内引用到input[type=file]")
		}
		// 2. 句柄转节点，取 BackendNodeID
		nodeID, err := dom.RequestNode(ro.ObjectID).Do(c)
		if err != nil {
			return err
		}
		node, err := dom.DescribeNode().WithNodeID(nodeID).Do(c)
		if err != nil || node == nil || node.BackendNodeID == 0 {
			return errors.New("解析input的BackendNodeID失败")
		}
		// 3. 直接注入文件（不依赖可见性/焦点/用户激活）
		if err := dom.SetFileInputFiles([]string{absVideoPath}).WithBackendNodeID(node.BackendNodeID).Do(c); err != nil {
			return err
		}
		logger.Print("WX3", fmt.Sprintf("已通过CDP直接注入文件到沙箱iframe的input(backend=%d)", node.BackendNodeID))
		return nil
	}))
	if err != nil {
		logger.Print("WX3", "CDP直接注入文件失败: "+err.Error())
		return false
	}
	// 4. 注入后同域复查 files 并派发事件：事件对象用 input 所属文档的构造函数创建（避免跨域 Event），返回详细诊断便于排查
	var diag string
	_ = fs.evaluate(&diag, `(function(){
		var els = document.querySelectorAll('input[type="file"]');
		for (var i = 0; i < els.length; i++) {
			var inp = els[i];
			if (!inp.files || inp.files.length === 0) continue;
			var names = [];
			try { for (var j = 0; j < inp.files.length; j++) names.push(inp.files[j].name); } catch(eN){}
			var d1 = false, d2 = false;
			try { d1 = inp.dispatchEvent(new inp.ownerDocument.defaultView.Event('change', {bubbles: true})); } catch(e){}
			try { d2 = inp.dispatchEvent(new inp.ownerDocument.defaultView.Event('input', {bubbles: true})); } catch(e2){}
			return 'files=' + inp.files.length + ' name=' + names.join('|').slice(0, 60) + ' changeDispatch=' + d1 + ' inputDispatch=' + d2;
		}
		// 无信号返回空串：跨框架包装器返回第一个非空结果，非空默认值会在主框架短路
		return '';
	})()`, 5*time.Second)
	logger.Print("WX3", "注入后复查: "+diag)
	return strings.HasPrefix(diag, "files=")
}

// resolveSandboxInputInfo 在主框架 JS 中定位同源沙箱 iframe 内的 file input，强制 iframe 与 input 可见，
// 返回诊断信息（含 input 中心点坐标，供 CDP 可信点击使用）。iframe 被强制铺满视口左上角，
// 故 iframe 内坐标可直接作为主视口坐标使用。
func resolveSandboxInputInfo(fs frameScope) string {
	var info string
	_ = fs.evaluate(&info, `(function(){
		var f = document.querySelector('iframe[name="content"]') || document.querySelector('iframe[data-wujie-flag]');
		if (!f) return 'no-iframe';
		f.style.setProperty('display', 'block', 'important');
		f.style.setProperty('visibility', 'visible', 'important');
		f.style.setProperty('opacity', '1', 'important');
		f.style.setProperty('position', 'absolute', 'important');
		f.style.setProperty('left', '0', 'important');
		f.style.setProperty('top', '0', 'important');
		f.style.setProperty('width', '100%', 'important');
		f.style.setProperty('height', '100%', 'important');
		f.style.setProperty('z-index', '-1', 'important');
		var cd = f.contentDocument;
		if (!cd) return 'iframe-no-doc';
		var inp = cd.querySelector('input[type="file"]');
		if (!inp) return 'iframe-no-input';
		// antd 隐藏代理 input 常为 display:none/0尺寸，强制可见可点击（尺寸极小不影响界面）
		inp.style.setProperty('display', 'block', 'important');
		inp.style.setProperty('visibility', 'visible', 'important');
		inp.style.setProperty('opacity', '1', 'important');
		inp.style.setProperty('position', 'fixed', 'important');
		inp.style.setProperty('left', '10px', 'important');
		inp.style.setProperty('top', '10px', 'important');
		inp.style.setProperty('width', '20px', 'important');
		inp.style.setProperty('height', '20px', 'important');
		inp.style.setProperty('z-index', '2147483647', 'important');
		var r = inp.getBoundingClientRect();
		return 'ok cx=' + Math.round(r.left + r.width / 2) + ' cy=' + Math.round(r.top + r.height / 2);
	})()`, 5*time.Second)
	return info
}

// trustedClickFileInput 强制沙箱 iframe 与隐藏 file input 可见后，用 CDP Input 派发真实鼠标事件点击 input：
// 可信点击会触发浏览器原生文件选择框（被拦截器接管填入文件，浏览器随后派发真实 change 事件，
// antd/rc-upload 必然响应）；合成 JS 点击/合成 change 事件实测均不被组件感知。
func trustedClickFileInput(fs frameScope, logger *logx.Logger) {
	info := resolveSandboxInputInfo(fs)
	logger.Print("WX3", "沙箱input定位: "+info)
	if !strings.HasPrefix(info, "ok") {
		return
	}
	var cx, cy float64
	for _, p := range strings.Fields(info) {
		if strings.HasPrefix(p, "cx=") {
			cx, _ = strconv.ParseFloat(strings.TrimPrefix(p, "cx="), 64)
		} else if strings.HasPrefix(p, "cy=") {
			cy, _ = strconv.ParseFloat(strings.TrimPrefix(p, "cy="), 64)
		}
	}
	if cx <= 0 || cy <= 0 {
		logger.Print("WX3", "可信点击坐标解析失败: "+info)
		return
	}
	clickCtx, cancel := context.WithTimeout(fs.ctx, 8*time.Second)
	err := chromedp.Run(clickCtx, chromedp.ActionFunc(func(c context.Context) error {
		if err := input.DispatchMouseEvent(input.MousePressed, cx, cy).WithButton(input.Left).WithClickCount(1).Do(c); err != nil {
			return err
		}
		return input.DispatchMouseEvent(input.MouseReleased, cx, cy).WithButton(input.Left).WithClickCount(1).Do(c)
	}))
	cancel()
	if err != nil {
		logger.Print("WX3", "可信点击file input失败: "+err.Error())
		return
	}
	logger.Print("WX3", fmt.Sprintf("已可信点击file input(cx=%.0f cy=%.0f)，等待原生文件选择框弹出", cx, cy))
}

// uploadStartedCrossFrame 跨框架检测上传是否已启动（页面出现上传进度/完成相关文案）
func uploadStartedCrossFrame(fs frameScope) bool {
	var started bool
	_ = fs.evaluate(&started, `(function(){
		// 上传中判定：仅依赖明确的上传进度文本（无信号返回空串，避免跨框架包装器在主框架短路）
		var body = document.body ? (document.body.innerText || '') : '';
		if (body.indexOf('正在上传') >= 0) return true;
		if (body.indexOf('重新上传') >= 0) return true;
		if (body.indexOf('更换封面') >= 0) return true;
		if (/上传[^\n]*?\d{1,3}%/.test(body)) return true;
		return false;
	})()`, 5*time.Second)
	return started
}

// queryOpts 返回元素查询选项：iframe 作用域下按 src 重新解析节点并追加 FromNode（仅限 ByQuery）；
// 解析失败回退主框架查询。
func (fs *frameScope) queryOpts(extra ...chromedp.QueryOption) []chromedp.QueryOption {
	if fs.iframeSrc == "" {
		return extra
	}
	node, err := fs.resolveNode()
	if err != nil || node == nil {
		return extra
	}
	return append([]chromedp.QueryOption{chromedp.FromNode(node)}, extra...)
}

// waitAndUploadFile 等待文件上传控件并选择本地视频。
// 主框架与 iframe 双作用域轮询：上传控件可能渲染在主框架，也可能在内容 iframe（初始 empty.html，稍后加载）内。
func waitAndUploadFile(fs frameScope, iframeFS frameScope, logger *logx.Logger, absVideoPath string) error {
	logger.Print("WX3", "等待视频上传控件")
	uploadSelectors := []string{
		`input[type="file"]`,
		`div[class*="upload"] input[type="file"]`,
		`div[class*="uploader"] input[type="file"]`,
		`input[accept*="video"]`,
		`input[accept="video/*"]`,
	}
	// 上传入口容器选择器（非 file input，点击后才会注入控件/弹文件选择框；来自实测页面结构）
	uploadBoxSelectors := []string{
		// `#container-wrap > div:nth-of-type(2) > div > div > div:nth-of-type(1) > div:nth-of-type(3) > div > div:nth-of-type(2) > div:nth-of-type(1) > div > div > div > span > div > span > div > div`,
		`.material .upload`,
	}

	// 开启文件选择框拦截：视频号助手点击上传入口会弹原生文件选择框（阻塞页面），拦截后由程序自动填入文件
	chooserDone := make(chan struct{}, 1)
	setupCtx, cancelSetup := context.WithTimeout(fs.ctx, 10*time.Second)
	err := chromedp.Run(setupCtx, page.Enable(), page.SetInterceptFileChooserDialog(true))
	cancelSetup()
	if err != nil {
		logger.Print("WX3", "开启文件选择框拦截失败: "+err.Error())
	} else {
		chromedp.ListenTarget(fs.ctx, func(ev interface{}) {
			e, ok := ev.(*page.EventFileChooserOpened)
			if !ok {
				return
			}
			logger.Print("WX3", "已拦截文件选择框，自动填入视频文件")
			go func(backendNodeID cdp.BackendNodeID) {
				fileCtx, cancelFile := context.WithTimeout(fs.ctx, 15*time.Second)
				defer cancelFile()
				// 新版 Chrome（151）已移除 Page.handleFileChooser：对 FileChooserOpened 事件的 backendNode 调
				// DOM.setFileInputFiles 即为接受选择框（Puppeteer FileChooser.accept 同机制），接受路径下浏览器会派发真实 change。
				if err := chromedp.Run(fileCtx, dom.SetFileInputFiles([]string{absVideoPath}).WithBackendNodeID(backendNodeID)); err != nil {
					logger.Print("WX3", "填入文件失败: "+err.Error())
					return
				}
				logger.Print("WX3", "已对选择框的input填入文件(接受路径，浏览器将派发真实change)")
				// 稍候复查：若组件已消费文件（真实change已触发）input 会被重置为空；未消费则手动补发 change。
				time.Sleep(800 * time.Millisecond)
				var postState string
				_ = fs.evaluate(&postState, `(function(){
					var out = [];
					var els = document.querySelectorAll('input[type="file"]');
					for (var i = 0; i < els.length; i++) {
						out.push('input' + i + '.files=' + (els[i].files ? els[i].files.length : -1));
					}
					if (els.length > 0) {
						for (var k = 0; k < els.length; k++) {
							if (els[k].files && els[k].files.length > 0) {
								try { els[k].dispatchEvent(new els[k].ownerDocument.defaultView.Event('change', {bubbles: true})); } catch(e){}
								try { els[k].dispatchEvent(new els[k].ownerDocument.defaultView.Event('input', {bubbles: true})); } catch(e2){}
								out.push('manualChangeDispatched');
							}
						}
					}
					var body = document.body ? (document.body.innerText || '') : '';
					if (body.indexOf('上传') >= 0 || body.indexOf('失败') >= 0 || body.indexOf('不支持') >= 0) {
						out.push('body=' + JSON.stringify(body.slice(0, 120)));
					}
					return out.join(' ');
				})()`, 5*time.Second)
				logger.Print("WX3", "填入后复查: "+postState)
				select {
				case chooserDone <- struct{}{}:
				default:
				}
			}(e.BackendNodeID)
		})
	}

	var found string
	var foundScope frameScope
	directInjected := false
	trustedClicked := false
	deadline := time.Now().Add(90 * time.Second)
	triggerTries := 0
	loopCount := 0
	diagDumped := false
	snapshotDumped := false
	waitStart := time.Now()
	// 双重等待机制：
	// 1. 通过 select 监听 chooserDone 通道，拦截器填入文件后还需验证上传真正启动（指纹浏览器内核可能不派发 change）。
	// 2. 否则，轮询查找 uploadSelectors 中定义的选择器，看文件上传控件是否出现在DOM中。
	for time.Now().Before(deadline) {
		loopCount++
		// 已通过拦截文件选择框填入文件：验证上传真正启动后返回（实测指纹浏览器下 setFileInputFiles 可能不触发前端响应）
		select {
		case <-chooserDone:
			logger.Print("WX3", "已通过文件选择框拦截完成文件填入，验证上传启动...")
			return finishAfterChooser(fs, logger, absVideoPath)
		default:
		}
		// 动态监测页面是否突发账号异常（仅匹配账号受限类专属文案）
		if kw := checkAnomalyText(fs); kw != "" {
			return fmt.Errorf("WX3 weixin channels account anomaly (matched '%s'): account restricted", kw)
		}

		// 跨框架探测（主文档+同源iframe+shadow roots）：wujie 子应用渲染在沙箱 iframe 内，
		// 主框架 CDP 选择器查询够不到，必须同源 JS 穿透
		if cf := findFileInputCrossFrame(fs, logger); cf != "" {
			logger.Print("WX3", "跨框架探测命中: "+cf)
			// 路线1：强制可见 + CDP 可信点击隐藏 file input → 浏览器弹原生文件选择框（拦截器填入文件后，
			// 浏览器会派发真实 change 事件，antd 组件必然响应——合成 change 事件实测不被组件感知）
			if !trustedClicked {
				trustedClicked = true
				trustedClickFileInput(fs, logger)
			}
			// 阻塞等待拦截器处理完成（handleFileChooser 需要处理窗口，不能用 default 立即放行）
			select {
			case <-chooserDone:
				logger.Print("WX3", "已通过文件选择框拦截完成文件填入，验证上传启动...")
				return finishAfterChooser(fs, logger, absVideoPath)
			case <-time.After(6 * time.Second):
			}
			// 路线2：选择框未弹出时，CDP 按 BackendNodeID 直接注入文件到沙箱 iframe 的 input（不依赖可见性/焦点）
			if !directInjected {
				directInjected = true
				injectFileCrossFrame(fs, logger, absVideoPath)
				time.Sleep(5 * time.Second)
				if uploadStartedCrossFrame(fs) {
					logger.Print("WX3", "上传已启动（注入路线生效）")
					return nil
				}
				logger.Print("WX3", "注入后5秒未检测到上传启动，尝试可信点击打开文件选择框")
				trustedClickFileInput(fs, logger)
				select {
				case <-chooserDone:
					logger.Print("WX3", "已通过文件选择框拦截完成文件填入，验证上传启动...")
					return finishAfterChooser(fs, logger, absVideoPath)
				case <-time.After(6 * time.Second):
				}
				logger.Print("WX3", "两条注入路线均未确认上传启动，交给WX4继续等待并复查")
				return nil
			}
		}

		// 强制 wujie 沙箱 iframe 可见（实测被页面设为 display:none：内容不可见且其内 JS 点击不弹文件选择框；
		// 置为底层背景层既恢复渲染/交互又不遮挡主界面）。可见后 CDP 常规点击/拦截路径即可生效。
		if loopCount%3 == 1 {
			var visInfo string
			_ = fs.evaluate(&visInfo, `(function(){
				var f = document.querySelector('iframe[data-wujie-flag], iframe[name="content"]');
				if (!f) return '';
				var st = window.getComputedStyle(f);
				var changed = false;
				if (st.display === 'none' || st.visibility === 'hidden' || f.offsetWidth === 0) {
					f.style.setProperty('display', 'block', 'important');
					f.style.setProperty('visibility', 'visible', 'important');
					f.style.setProperty('opacity', '1', 'important');
					f.style.setProperty('position', 'absolute', 'important');
					f.style.setProperty('left', '0', 'important');
					f.style.setProperty('top', '0', 'important');
					f.style.setProperty('width', '100%', 'important');
					f.style.setProperty('height', '100%', 'important');
					f.style.setProperty('z-index', '-1', 'important');
					changed = true;
				}
				var shadowKids = -1;
				var wa = document.querySelector('wujie-app');
				try { shadowKids = wa && wa.shadowRoot ? wa.shadowRoot.childNodes.length : -2; } catch(e){}
				return (changed ? 'forced-visible ' : 'already-visible ') + 'iframe尺寸=' + f.offsetWidth + 'x' + f.offsetHeight + ' shadow子=' + shadowKids;
			})()`, 5*time.Second)
			if visInfo != "" {
				logger.Print("WX3", "沙箱iframe可见性处理: "+visInfo)
			}
		}

		// 等待约15秒仍无进展时输出一次页面诊断（主框架与跨框架全景分别输出），便于提前定位上传控件缺失原因。
		// 上传区由 wujie 微前端子应用 JS 渲染，可能延迟挂载，诊断同时列出容器子树结构辅助判断。
		dumpDiag := func() {
			logger.Print("WX3", "===== 主框架诊断 =====")
			dumpUploadDiagnostics(fs, logger)
			dumpRenderStructure(fs, logger)
			logger.Print("WX3", "===== 跨框架全景诊断 =====")
			dumpCrossFrameOverview(fs, logger)
		}
		if !diagDumped && time.Since(waitStart) > 15*time.Second {
			dumpDiag()
			diagDumped = true
		}
		// 30秒后再输出一次快照：子应用渲染较慢时用于对比容器内容是否有变化
		snapshotDue := !snapshotDumped && time.Since(waitStart) > 30*time.Second
		if snapshotDue {
			logger.Print("WX3", "30秒快照（渲染对比）:")
			dumpMainSnapshot(fs.ctx, logger)
			dumpDiag()
			dumpWujieState(fs, logger)
			snapshotDumped = true
		}

		// 部分页面需先点击"上传"入口才会注入 file input：主框架、iframe 依次尝试（最多5次）
		if triggerTries < 5 && loopCount%3 == 1 {
			triggerTries++
			boxClicked := false
			for _, scope := range []frameScope{fs, iframeFS} {
				if boxClicked {
					break
				}
				for _, box := range uploadBoxSelectors {
					boxCtx, cancelBox := context.WithTimeout(scope.ctx, 1500*time.Millisecond)
					var boxNodes []*cdp.Node
					_ = chromedp.Run(boxCtx, chromedp.Nodes(box, &boxNodes, scope.queryOpts(chromedp.ByQuery)...))
					cancelBox()
					if len(boxNodes) == 0 {
						logger.Print("WX3", "未检测到上传入口容器: "+box)
						continue
					}
					logger.Print("WX3", fmt.Sprintf("检测到上传入口容器存在 (匹配 %d 个): %s", len(boxNodes), box))
					clickCtx, cancelClick := context.WithTimeout(scope.ctx, 5*time.Second)
					err := chromedp.Run(clickCtx, chromedp.ScrollIntoView(box, scope.queryOpts(chromedp.ByQuery)...), chromedp.Click(box, scope.queryOpts(chromedp.ByQuery)...))
					cancelClick()
					if err == nil {
						logger.Print("WX3", "已点击上传入口容器")
						boxClicked = true
						break
					}
					logger.Print("WX3", "点击上传入口容器失败: "+err.Error())
				}
			}
			if !boxClicked {
				if t := clickUploadTrigger(fs, iframeFS); t != "" {
					logger.Print("WX3", "已按文案点击上传入口: "+t)
				}
			}
		}
		// 轮询查找 uploadSelectors 中定义的选择器（CSS）：主框架优先，其次 iframe，看文件上传控件是否出现在DOM中
	scopeLoop:
		for _, scope := range []frameScope{fs, iframeFS} {
			for _, sel := range uploadSelectors {
				checkCtx, cancel := context.WithTimeout(scope.ctx, 1500*time.Millisecond)
				var nodes []*cdp.Node
				_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, scope.queryOpts(chromedp.ByQuery)...))
				cancel()
				if len(nodes) > 0 {
					found = sel
					foundScope = scope
					break scopeLoop
				}
			}
		}
		if found != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if found == "" {
		logger.Print("WX3", "===== 主框架诊断 =====")
		dumpUploadDiagnostics(fs, logger)
		dumpRenderStructure(fs, logger)
		dumpWujieState(fs, logger)
		logger.Print("WX3", "===== 跨框架全景诊断 =====")
		dumpCrossFrameOverview(fs, logger)
		return errors.New("WX3 video upload control not found within 90 seconds (页面上传区由JS子应用渲染，若主框架诊断中容器无内容，说明子应用未挂载成功，请查看日志中的控制台错误与网络失败)")
	}

	logger.Print("WX3", "使用选择器: "+found)

	uploadCtx, cancelUpload := context.WithTimeout(foundScope.ctx, 20*time.Second)
	defer cancelUpload()

	if err := chromedp.Run(uploadCtx, chromedp.WaitReady(found, foundScope.queryOpts(chromedp.ByQuery)...)); err != nil {
		return checkAnomalyContext(foundScope, fmt.Errorf("WX3 wait upload input ready failed: %v", err))
	}

	logger.Print("WX4", "开始选择视频文件: "+absVideoPath)
	if err := chromedp.Run(uploadCtx, chromedp.SetUploadFiles(found, []string{absVideoPath}, foundScope.queryOpts(chromedp.ByQuery)...)); err != nil {
		return checkAnomalyContext(foundScope, fmt.Errorf("WX4 set upload files failed: %v", err))
	}
	return nil
}

// clickUploadTrigger 按文案点击页面上的上传入口：依次在主框架、iframe 作用域内尝试，
// 取含"上传"文案的可见元素中面积最小的一个（通常是内层控件），返回被点击的文案（未找到返回空）
func clickUploadTrigger(fs frameScope, iframeFS frameScope) string {
	var clicked string
	js := `(function(){
		var xr = document.evaluate("//*[contains(text(),'上传')]", document, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
		var candidates = [];
		for (var i = 0; i < xr.snapshotLength; i++) {
			var el = xr.snapshotItem(i);
			if (el.offsetHeight <= 0 || el.offsetWidth <= 0) continue;
			var t = ((el.innerText || el.textContent || '') + '').trim();
			if (t.length > 30) continue; // 跳过大容器，只留短文案控件（如"上传视频"）
			candidates.push(el);
		}
		if (candidates.length === 0) return '';
		// 优先最小元素（上传入口通常是内层控件）
		candidates.sort(function(a, b){
			return (a.offsetWidth * a.offsetHeight) - (b.offsetWidth * b.offsetHeight);
		});
		var el = candidates[0];
		var t = ((el.innerText || el.textContent || '') + '').trim();
		try { el.scrollIntoView({block:'center'}); } catch (e) {}
		try { el.click(); } catch (e) { return ''; }
		return t;
	})()`
	for _, scope := range []frameScope{fs, iframeFS} {
		clicked = ""
		_ = scope.evaluate(&clicked, js, 3*time.Second)
		if clicked != "" {
			return clicked
		}
	}
	return ""
}

// dumpUploadDiagnostics 找不到上传控件时，输出页面诊断信息（input/iframe/含"上传"文案的元素）便于排查。
// 在 iframe 作用域下输出的是 iframe 内的 DOM 情况。
func dumpUploadDiagnostics(fs frameScope, logger *logx.Logger) {
	var info string
	js := `(function(){
		var out = [];
		out.push('当前文档URL=' + location.href);
		out.push('title=' + document.title);
		out.push('body文本预览=' + JSON.stringify((document.body ? (document.body.innerText || '') : '').slice(0, 200)));
		var inputs = document.querySelectorAll('input');
		out.push('input总数=' + inputs.length);
		for (var i = 0; i < inputs.length && i < 10; i++) {
			out.push('input[' + i + '] type=' + (inputs[i].type || '') + ' accept=' + (inputs[i].accept || '') + ' class=' + (('' + (inputs[i].className || '')).slice(0, 80)));
		}
		out.push('iframe总数=' + document.querySelectorAll('iframe').length);
		var xr = document.evaluate("//*[contains(text(),'上传')]", document, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
		out.push('含"上传"文案的元素数=' + xr.snapshotLength);
		for (var j = 0; j < xr.snapshotLength && j < 15; j++) {
			var n = xr.snapshotItem(j);
			out.push('upload[' + j + '] tag=' + n.tagName + ' class=' + (('' + (n.className || '')).slice(0, 80)) + ' text=' + (('' + (n.innerText || n.textContent || '')).trim().slice(0, 40)));
		}
		return out.join('\n');
	})()`
	if err := fs.evaluate(&info, js, 5*time.Second); err != nil {
		logger.Print("WX3", "页面诊断失败: "+err.Error())
		return
	}
	logger.Print("WX3", "页面诊断信息:\n"+info)
}

// waitForUploadComplete 智能等待视频上传完成。
// 检测条件：上传进度文本消失 + 封面编辑区/发表按钮出现。
// 轮询期间主动关闭引导弹窗，防止弹窗遮挡导致流程卡住。
// 最长等待 5 分钟。
func waitForUploadComplete(fs frameScope, logger *logx.Logger) error {
	logger.Print("WX4", "智能等待视频上传完成...")
	deadline := time.Now().Add(5 * time.Minute)
	loopCount := 0

	for time.Now().Before(deadline) {
		loopCount++

		// 每 10 轮尝试关闭引导弹窗（如"知道了"按钮）；不 continue，弹窗点击后仍继续检查上传状态，
		// 避免弹窗持续存在时循环被占满、上传状态永远得不到检查（实测曾因此卡满 5 分钟）
		if loopCount%10 == 1 {
			var popupClicked bool
			_ = fs.evaluate(&popupClicked, `(function(){
				var btns = document.querySelectorAll('button, div[role="button"]');
				for(var i=0;i<btns.length;i++){
					var t = (btns[i].innerText||"").trim();
					if(btns[i].disabled || btns[i].getAttribute('aria-disabled')==='true') continue;
					if(t==='知道了' || t==='我知道了' || t==='不再提示'){
						try{btns[i].click();}catch(e){}
						return true;
					}
				}
				return false;
			})()`, 3*time.Second)
			if popupClicked {
				logger.Print("WX4", "已尝试关闭引导弹窗")
			}
		}

		// 一次性获取页面状态：是否上传中 + 是否已完成（合并成一次调用，减少 CDP 往返）。
		// 注意：跨框架包装器返回第一个非空结果，无信号时必须返回空串（而非 "waiting"），
		// 否则主框架的非空返回值会短路、永远轮不到沙箱 iframe（实测曾因此恒为 waiting）
		var state string
		_ = fs.evaluate(&state, `(function(){
			var body = document.body ? (document.body.innerText || "") : "";
			// 上传中判定：仅依赖明确的上传进度文本
			if(body.indexOf("正在上传") >= 0) return "uploading";
			if(body.indexOf("上传中") >= 0 && body.indexOf("上传中，") < 0 && body.indexOf("重新上传") < 0) return "uploading";
			var m = body.match(/\u4e0a\u4f20[^\n]*?(\d{1,3})%/);
			if(m && m[1] !== "100") return "uploading";
			// 完成判定：上传完成后才会出现的页面元素（视频号助手发表页）
			if(body.indexOf("重新上传") >= 0) return "done";
			if(body.indexOf("更换封面") >= 0) return "done";
			if(body.indexOf("编辑封面") >= 0) return "done";
			if(body.indexOf("已上传") >= 0) return "done";
			// 发表按钮已存在且未禁用（上传完成且视频处理完毕）
			var btns = document.querySelectorAll('button');
			for(var i=0;i<btns.length;i++){
				var t = (btns[i].innerText||"").trim();
				if(t==='发表' && !btns[i].disabled){ return "done"; }
			}
			return "";
		})()`, 3*time.Second)

		if state == "done" {
			logger.Print("WX4", "视频上传完成，检测到封面编辑区/发表按钮已就绪")
			return nil
		}
		if state == "" {
			state = "waiting"
		}

		// 每 5 轮(~15秒)输出一次状态，方便排查卡住原因；持续 waiting 时附页面诊断（指纹浏览器环境定位关键）
		if loopCount%5 == 0 {
			logger.Print("WX4", fmt.Sprintf("上传状态轮询中: state=%s (已等待%ds)", state, loopCount*3))
			if state == "waiting" && loopCount%10 == 0 {
				logger.Print("WX4", "页面诊断: "+uploadDiagnostics(fs))
			}
		}

		time.Sleep(3 * time.Second)
	}

	// 超时后不报错，继续流程（可能是小视频已上传完成但检测未命中）
	logger.Print("WX4", "等待上传超时(5分钟)，继续执行后续流程")
	logger.Print("WX4", "超时诊断: "+uploadDiagnostics(fs))
	return nil
}

// uploadDiagnostics 输出沙箱内当前页面状态摘要（body前150字、file input数、按钮文案），供卡住时定位。
func uploadDiagnostics(fs frameScope) string {
	var info string
	_ = fs.evaluate(&info, `(function(){
		var body = document.body ? (document.body.innerText || '') : '';
		var inputs = document.querySelectorAll('input[type="file"]').length;
		var btns = [];
		var bs = document.querySelectorAll('button');
		for (var i = 0; i < bs.length && btns.length < 6; i++) {
			var t = ((bs[i].innerText || '') + '').trim();
			if (t && t.length < 12) btns.push(t);
		}
		return 'body[' + body.replace(/\s+/g, ' ').slice(0, 150) + '] fileInputs=' + inputs + ' buttons=[' + btns.join(',') + ']';
	})()`, 5*time.Second)
	if info == "" {
		return "沙箱内无任何可诊断内容(子应用可能未渲染或不可达)"
	}
	return info
}

// confirmUploadStarted 轮询检测上传启动信号：页面文案（正在上传/进度）或网络层分片请求任一命中即算启动。
func confirmUploadStarted(fs frameScope, tries int) bool {
	for i := 0; i < tries; i++ {
		if uploadStartedCrossFrame(fs) || atomic.LoadInt32(&uploadNetSignal) == 1 {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// dispatchChangeCrossFrame 对沙箱内仍持有文件的 file input 补发 change/input 事件（兜底：
// 部分内核 setFileInputFiles 接受路径不派发真实 change，前端组件无感知）。
func dispatchChangeCrossFrame(fs frameScope, logger *logx.Logger) {
	var info string
	_ = fs.evaluate(&info, `(function(){
		var els = document.querySelectorAll('input[type="file"]');
		for (var i = 0; i < els.length; i++) {
			if (els[i].files && els[i].files.length > 0) {
				var W = els[i].ownerDocument.defaultView;
				try { els[i].dispatchEvent(new W.Event('change', {bubbles: true})); } catch(e){}
				try { els[i].dispatchEvent(new W.Event('input', {bubbles: true})); } catch(e2){}
				return 'dispatched input.files=' + els[i].files.length;
			}
		}
		return '';
	})()`, 5*time.Second)
	if info != "" {
		logger.Print("WX3", "补发change事件: "+info)
	}
}

// finishAfterChooser 选择框拦截填入完成后的统一收尾：验证上传启动（页面文案或网络层分片请求）。
// 未确认时仅补发 change（幂等）；不再重复注入文件/再次点击——重复给文件会触发前端"切换"弹窗并卡死上传状态机。
func finishAfterChooser(fs frameScope, logger *logx.Logger, absVideoPath string) error {
	if atomic.LoadInt32(&uploadNetSignal) == 1 {
		logger.Print("WX3", "上传已启动（网络层检测到上传分片请求，选择框路线生效）")
		return nil
	}
	if confirmUploadStarted(fs, 6) {
		logger.Print("WX3", "上传已启动（选择框路线生效）")
		return nil
	}
	logger.Print("WX3", "填入后12秒未检测到上传启动（页面文案与网络层均无信号），补发change兜底")
	dispatchChangeCrossFrame(fs, logger)
	if confirmUploadStarted(fs, 4) {
		logger.Print("WX3", "上传已启动（补发change生效）")
		return nil
	}
	logger.Print("WX3", "【警告】未确认上传启动，但不再重复注入文件（重复给文件会触发\"切换\"弹窗并卡死前端上传状态机），交WX4继续等待 诊断: "+uploadDiagnostics(fs))
	return nil
}

// dismissPopups 关闭上传过程中的提示弹窗（在作用域框架内用 JS 查找并点击）。
// "知道了"类安全文案直接点击；"确定/关闭/取消/跳过"仅在弹窗容器内点击，避免误伤功能按钮。
func dismissPopups(fs frameScope, logger *logx.Logger) error {
	logger.Print("WX9", "尝试关闭提示窗口")
	js := `(function(){
		function inModal(el){
			var p = el;
			for (var k = 0; k < 8 && p; k++) {
				var cls = ((p.className || '') + '');
				if (cls.indexOf('modal') >= 0 || cls.indexOf('dialog') >= 0 || cls.indexOf('popup') >= 0 || cls.indexOf('mask') >= 0 || cls.indexOf('popover') >= 0) return true;
				p = p.parentElement;
			}
			return false;
		}
		var safe = ['知道了', '我知道了', '不再提示'];
		var modalOnly = ['确定', '关闭', '取消', '跳过'];
		var clicked = false;
		var btns = document.querySelectorAll('button, div[role="button"], a');
		for (var i = 0; i < btns.length; i++) {
			var b = btns[i];
			if (b.disabled) continue;
			if (b.offsetHeight <= 0 || b.offsetWidth <= 0) continue;
			var t = ((b.innerText || '') + '').trim();
			if (t.length === 0 || t.length > 8) continue;
			var isSafe = safe.indexOf(t) >= 0;
			var isModalOnly = modalOnly.indexOf(t) >= 0 && inModal(b);
			if (isSafe || isModalOnly) {
				try { b.click(); clicked = true; } catch (e) {}
			}
		}
		var closes = document.querySelectorAll('div[class*="close"], span[class*="close"], i[class*="close"]');
		for (var j = 0; j < closes.length; j++) {
			var c = closes[j];
			if (c.offsetHeight <= 0 || c.offsetWidth <= 0) continue;
			if (inModal(c)) { try { c.click(); clicked = true; } catch (e) {} }
		}
		return clicked;
	})()`
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var clicked bool
		if err := fs.evaluate(&clicked, js, 3*time.Second); err == nil && clicked {
			logger.Print("WX9", "已关闭提示")
			time.Sleep(800 * time.Millisecond)
			continue
		}
		time.Sleep(600 * time.Millisecond)
	}
	return nil
}

// fillText 填写视频描述/文案（在作用域框架内执行）
func fillText(fs frameScope, logger *logx.Logger, text string) error {
	logger.Print("WX5", "========== 开始填写视频文案 ==========")
	logger.Print("WX5", "待填写文本长度: "+strconv.Itoa(len(text))+" 字符")
	logger.Print("WX5", "文本预览(前100字符): "+text[:min(len(text), 100)])

	time.Sleep(6 * time.Second)
	if checkSomethingWentWrong(fs) {
		logger.Print("WX5", "【错误】在等待6秒后检测到页面错误")
		return errors.New("WX5 page error detected after initial sleep")
	}

	// 视频号助手的描述输入框选择器
	titleSelectors := []string{
		`div[contenteditable='true'][data-placeholder]`,
		`div[class*='editor'] [contenteditable='true']`,
		`div[class*='desc'] [contenteditable='true']`,
		`div[class*='caption'] [contenteditable='true']`,
		`div[contenteditable='true']`,
		`textarea[placeholder*='描述']`,
		`textarea[placeholder*='标题']`,
		`textarea`,
	}

	var foundSel string

	// 查找可用的描述输入框（CSS 选择器，支持 FromNode 进入 iframe）；
	// 跨框架作用域下 CDP 选择器够不到沙箱 iframe，跳过逐个试探直接走 JS 检测（省 ~30秒）
	for _, sel := range titleSelectors {
		if fs.iframeSrc != "" {
			break
		}
		checkCtx, cancel := context.WithTimeout(fs.ctx, 3*time.Second)
		var nodes []*cdp.Node
		_ = chromedp.Run(checkCtx, chromedp.Nodes(sel, &nodes, fs.queryOpts(chromedp.ByQuery)...))
		cancel()
		if len(nodes) > 0 {
			foundSel = sel
			break
		}
	}

	if foundSel == "" {
		// 兜底：JS 枚举所有可编辑候选（contenteditable/textarea），挑选可见且面积最大的并打上唯一标记。
		// 不能直接 querySelector('textarea')：返回 document 顺序第一个，可能命中隐藏的代理 textarea，
		// 写入后页面真实编辑器无任何反应（曾导致空描述发布）。
		logger.Print("WX5", "未通过选择器找到描述输入框，尝试JS兜底枚举候选")
		var pickInfo string
		js := `(function(){
			// [contenteditable] 不限定属性值：plaintext-only/空串也是可编辑（[contenteditable="true"] 会漏判）
			var list = document.querySelectorAll('[contenteditable], textarea');
			if (!list.length) return '';
			var info = [];
			var base = (document.URL || '').slice(0, 70);
			var best = null; var bestArea = -1;
			var bestInvisible = null; var bestInvArea = -1;
			for (var i = 0; i < list.length; i++) {
				var el = list[i];
				// isContentEditable 对 contenteditable="true"/"plaintext-only"/"" 均为 true，属性判定不可靠时以它为准
				var editable = el.isContentEditable || el.tagName === 'TEXTAREA';
				if (!editable) continue;
				var r = el.getBoundingClientRect();
				var cs = el.ownerDocument.defaultView.getComputedStyle(el);
				var area = (r.width || 0) * (r.height || 0);
				var visible = el.offsetWidth > 0 && el.offsetHeight > 0 && cs.visibility !== 'hidden' && cs.display !== 'none';
				info.push(el.tagName + '|' + (((el.className || '') + '').slice(0, 60)) + '|ce=' + el.getAttribute('contenteditable') + '|area=' + Math.round(area) + '|vis=' + visible);
				if (visible && area > bestArea) { best = el; bestArea = area; }
				if (area > bestInvArea) { bestInvisible = el; bestInvArea = area; }
			}
			var picked = best; // 仅从可见候选中挑：本根只有隐藏候选时返回空串，让包装器继续遍历后续根（避免主文档隐藏 textarea-body 短路）
			if (!picked) return '';
			try { picked.setAttribute('__wx_desc_target', '1'); } catch(e) { return ''; }
			return 'picked=' + picked.tagName + '|area=' + Math.round(bestArea) + '|vis=true|base=' + base + ' || 候选: ' + info.join(' ;; ');
		})()`
		if err := fs.evaluate(&pickInfo, js, 5*time.Second); err != nil || pickInfo == "" {
			// 放宽：无可见候选时直接进沙箱 iframe 的 contentDocument 枚举（同源可穿透），按面积最大挑，不限可见性。
			// 不用跨框架包装器：主文档的隐藏候选会先短路，轮不到 iframe。
			logger.Print("WX5", "无可见编辑器候选，尝试直接穿透沙箱iframe枚举")
			looseJs := `(function(){
				var f = document.querySelector('iframe[name="content"]') || document.querySelector('iframe[data-wujie-flag]');
				var cd = null;
				try { cd = f && f.contentDocument; } catch(e) {}
				if (!cd) return '';
				var list = cd.querySelectorAll('[contenteditable], textarea');
				if (!list.length) return '';
				var info = []; var best = null; var bestArea = -1;
				for (var i = 0; i < list.length; i++) {
					var el = list[i];
					if (!(el.isContentEditable || el.tagName === 'TEXTAREA')) continue;
					var r = el.getBoundingClientRect();
					var area = (r.width || 0) * (r.height || 0);
					info.push(el.tagName + '|' + (((el.className || '') + '').slice(0, 60)) + '|area=' + Math.round(area));
					if (area > bestArea) { best = el; bestArea = area; }
				}
				if (!best) return '';
				try { best.setAttribute('__wx_desc_target', '1'); } catch(e2) { return ''; }
				return 'picked=' + best.tagName + '|area=' + Math.round(bestArea) + '|via=sandbox || 候选: ' + info.join(' ;; ');
			})()`
			mainCtx, cancelLoose := context.WithTimeout(fs.ctx, 5*time.Second)
			looseErr := chromedp.Run(mainCtx, chromedp.Evaluate(looseJs, &pickInfo))
			cancelLoose()
			if looseErr != nil || pickInfo == "" {
				return errors.New("WX5 cannot find weixin channels description input")
			}
		}
		logger.Print("WX5", "JS候选枚举: "+pickInfo)
		foundSel = `[__wx_desc_target='1']`
	}

	logger.Print("WX5", "定位描述容器: "+foundSel)

	// 跨框架作用域（wujie 沙箱 iframe）：CDP 选择器/WaitVisible 够不到，直接同源 JS 注入文本
	if fs.iframeSrc != "" {
		return fillTextViaJS(fs, logger, foundSel, text)
	}

	// Step1: 等待元素可见并点击获取焦点
	opts := fs.queryOpts(chromedp.ByQuery)
	stepCtx, cancel := context.WithTimeout(fs.ctx, 12*time.Second)
	err := chromedp.Run(stepCtx,
		chromedp.WaitVisible(foundSel, opts...),
		chromedp.Click(foundSel, opts...),
		chromedp.Focus(foundSel, opts...),
	)
	cancel()
	if err != nil {
		logger.Print("WX5", "Step1 失败: 未找到描述元素 - "+err.Error())
		return errors.New("WX5 cannot find weixin channels caption input")
	}
	logger.Print("WX5", "Step1 完成: 已点击并获取焦点")

	time.Sleep(3 * time.Second)

	// Step2: 输入文本
	logger.Print("WX5", "Step2: 输入文本内容")

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
	typeCtx, cancelType := context.WithTimeout(fs.ctx, 10*time.Second)
	err = chromedp.Run(typeCtx,
		chromedp.Click(foundSel, opts...),
		chromedp.Focus(foundSel, opts...),
	)
	cancelType()
	if err == nil {
		_ = fs.evaluate(&selectOk, selectJs, 5*time.Second)
	}
	if err == nil && selectOk {
		keysCtx, cancelKeys := context.WithTimeout(fs.ctx, 10*time.Second)
		err = chromedp.Run(keysCtx, chromedp.SendKeys(foundSel, text, opts...))
		cancelKeys()
	}
	if err != nil || !selectOk {
		if err != nil {
			logger.Print("WX5", "Step2 警告: SendKeys 执行异常 - "+err.Error())
		} else {
			logger.Print("WX5", "Step2 警告: JavaScript 全选失败")
		}
		// 兜底：使用 JS insertText（在作用域框架内）
		logger.Print("WX5", "Step2: 尝试使用 JavaScript 插入文本")
		var inputOk bool
		inputJs := fmt.Sprintf(`(function(T){
			var el=document.querySelector(%q);
			if(!el){return false;}
			el.focus();
			try{document.execCommand('selectAll', false, null);}catch(e){}
			try{document.execCommand('insertText', false, T);}catch(e){}
			return true;
		})(%q)`, foundSel, text)
		if err := fs.evaluate(&inputOk, inputJs, 5*time.Second); err != nil {
			logger.Print("WX5", "Step2 失败: JavaScript 插入也失败 - "+err.Error())
			return errors.New("WX5 cannot input text via SendKeys or JavaScript")
		}
		if !inputOk {
			logger.Print("WX5", "Step2 失败: JavaScript 插入返回 false")
			return errors.New("WX5 JavaScript input returned false")
		}
		logger.Print("WX5", "Step2 完成: 使用 JavaScript 插入文本成功")
	} else {
		logger.Print("WX5", "Step2 完成: 使用 JavaScript 全选 + SendKeys 输入成功")
	}

	// 验证最终文本（在作用域框架内）
	var finalText string
	_ = fs.evaluate(&finalText, fmt.Sprintf(`(function(sel){
		var el=document.querySelector(sel);
		return el ? el.textContent : '';
	})(%q)`, foundSel), 3*time.Second)
	logger.Print("WX5", "Step2: 最终文本内容(前100字符): "+finalText[:min(len(finalText), 100)])

	time.Sleep(3 * time.Second)

	if checkSomethingWentWrong(fs) {
		logger.Print("WX5", "【错误】在输入后检测到页面错误")
		return errors.New("WX5 page error detected after input")
	}

	logger.Print("WX5", "========== 填写视频文案完成 ==========")
	return nil
}

// fillTextViaJS 纯 JS 填写描述文本（跨框架同源穿透）：逐根查找选择器命中的编辑器，
// focus + selectAll 后 execCommand('insertText')（能触发框架的 input 事件）；
// 微信编辑器为富文本编辑器，兜底时直接写 textContent 并派发 InputEvent（普通 Event 不被识别）。
func fillTextViaJS(fs frameScope, logger *logx.Logger, sel, text string) error {
	inputJs := fmt.Sprintf(`(function(T){
		var el = document.querySelector(%q);
		if (!el) return '';
		try {
			// textarea/input 分支：execCommand('insertText') 是原生编辑命令，会触发真实
			// InputEvent(inputType=insertText)，React/受控组件必然感知（原生 setter+合成 Event 实测不被微信编辑器识别）
			if (el.tagName === 'TEXTAREA' || el.tagName === 'INPUT') {
				var docTa = el.ownerDocument;
				el.focus();
				try { el.setSelectionRange(0, (el.value || '').length); } catch(eSel){}
				var okTa = false;
				try { okTa = docTa.execCommand('insertText', false, T); } catch(eCmd){}
				var v = el.value || '';
				if (v.indexOf(T.slice(0, 10)) < 0) {
					// 兜底：原生 setter + InputEvent(inputType=insertText)
					var proto = docTa.defaultView.HTMLTextAreaElement.prototype;
					var setter = Object.getOwnPropertyDescriptor(proto, 'value');
					if (setter && setter.set) { setter.set.call(el, T); } else { el.value = T; }
					try {
						var IEta = docTa.defaultView.InputEvent;
						el.dispatchEvent(new IEta('input', {bubbles: true, inputType: 'insertText', data: T}));
					} catch(eIE) {
						try { el.dispatchEvent(new docTa.defaultView.Event('input', {bubbles: true})); } catch(eIE2){}
					}
					v = el.value || '';
				}
				return v.indexOf(T.slice(0, 10)) >= 0 ? ('ok textarea execCommand=' + okTa + ' len=' + v.length) : 'ta-insert-failed';
			}
			el.focus();
			try {
				var doc = el.ownerDocument;
				var win = doc.defaultView;
				var selection = win.getSelection();
				if (selection) {
					selection.removeAllRanges();
					var range = doc.createRange();
					range.selectNodeContents(el);
					selection.addRange(range);
				}
			} catch(eS){}
			var ok = false;
			try { ok = document.execCommand('insertText', false, T); } catch(e){}
			var hasText = (el.textContent || '').indexOf(T.slice(0, 10)) >= 0;
			if (!hasText) {
				// 兜底：直接写入 + 派发 InputEvent（富文本编辑器需 inputType 识别）
				try { el.textContent = T; } catch(e2){}
				try {
					var IE = el.ownerDocument.defaultView.InputEvent;
					el.dispatchEvent(new IE('input', {bubbles: true, inputType: 'insertText', data: T}));
				} catch(e3){
					try { el.dispatchEvent(new el.ownerDocument.defaultView.Event('input', {bubbles: true})); } catch(e4){}
				}
				hasText = (el.textContent || '').indexOf(T.slice(0, 10)) >= 0;
			}
			if (ok || hasText) return 'ok execCommand=' + ok + ' textLen=' + (el.textContent || '').length;
			return 'inserted-failed textLen=' + (el.textContent || '').length;
		} catch(eAll) {
			return 'err:' + eAll.message;
		}
	})(%q)`, sel, text)
	var result string
	if err := fs.evaluate(&result, inputJs, 8*time.Second); err != nil {
		logger.Print("WX5", "JS填写描述失败: "+err.Error())
		return errors.New("WX5 cannot input text via JavaScript (cross-frame)")
	}
	if !strings.HasPrefix(result, "ok") {
		logger.Print("WX5", "JS填写未成功，返回: "+result)
		return errors.New("WX5 JavaScript input failed (cross-frame): " + result)
	}
	logger.Print("WX5", "JS填写结果: "+result)
	// 验证最终文本（跨框架，兼容 textarea）
	var finalText string
	_ = fs.evaluate(&finalText, fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if (!el) return '';
		return el.tagName === 'TEXTAREA' ? el.value : el.textContent;
	})()`, sel), 3*time.Second)
	logger.Print("WX5", "JS填写完成，最终文本(前100字符): "+finalText[:min(len(finalText), 100)])
	if strings.TrimSpace(finalText) == "" {
		logger.Print("WX5", "【错误】填写后编辑器内容为空，中止流程（避免发布空描述视频）")
		return errors.New("WX5 description editor still empty after fill")
	}
	// 给框架一点时间消化 input 事件、更新状态（受控组件渲染有延迟），再进入发表步骤
	time.Sleep(2 * time.Second)
	logger.Print("WX5", "========== 填写视频文案完成 ==========")
	return nil
}

// findPublishButtonJS 在作用域内查找"发表"按钮（去空白后精确匹配文案，排除"定时发表"/"发表预览"）。
// 返回 'found'（可点击，已滚入视口中心并打唯一标记）或 'disabled'（按钮存在但禁用，通常是视频还在上传/转码）；
// 无任何候选返回 ''（跨框架包装器会继续遍历后续根）。
const findPublishButtonJS = `(function(){
	var els = document.querySelectorAll('button, div[role="button"], span[role="button"], a');
	var hasDisabled = false;
	for (var i = 0; i < els.length; i++) {
		var el = els[i];
		var t = (((el.innerText || '') + '').replace(/\s+/g, '')).trim();
		if (t !== '发表') continue;
		var cls = ((el.className || '') + '');
		var dis = el.disabled || el.getAttribute('aria-disabled') === 'true' || cls.indexOf('disabled') >= 0;
		if (dis) { hasDisabled = true; continue; }
		try { el.scrollIntoView({block:'center', inline:'center'}); } catch (e) {}
		try { el.setAttribute('__wx_pub_target', '1'); } catch (e2) {}
		return 'found';
	}
	return hasDisabled ? 'disabled' : '';
})()`

// clickPublish 查找并点击"发表"按钮。优先 CDP 可信点击（真实鼠标事件，指纹浏览器内核下合成
// el.click() 实测会被页面静默忽略）；找不到坐标时兜底合成点击。按钮在页面右侧需先滚入视口。
// 按钮存在但禁用（视频仍在上传/转码）时持续等待其可点击，最长 5 分钟。
func clickPublish(fs frameScope, logger *logx.Logger) error {
	logger.Print("WX6", "查找发表按钮")
	deadline := time.Now().Add(5 * time.Minute)
	disabledNoticed := false
	lastDiag := time.Time{}
	for time.Now().Before(deadline) {
		var found string
		_ = fs.evaluate(&found, findPublishButtonJS, 3*time.Second)
		if found == "found" {
			if trustedClickPublishButton(fs, logger) {
				return nil
			}
			// 可信点击不可用（取不到坐标等）：兜底合成点击（本地真 Chrome 环境验证过可用）
			var ok bool
			_ = fs.evaluate(&ok, `(function(){
				var el = document.querySelector('[__wx_pub_target]') || document.querySelector('button');
				var els = document.querySelectorAll('button, div[role="button"], span[role="button"]');
				for (var i = 0; i < els.length; i++) {
					var t = ((els[i].innerText || '') + '').trim();
					if (t === '发表' && !els[i].disabled) { try { els[i].click(); return true; } catch(e) { return false; } }
				}
				return false;
			})()`, 3*time.Second)
			if ok {
				logger.Print("WX6", "已合成点击发表按钮(兜底路线)")
				return nil
			}
		}
		// 按钮存在但禁用：视频上传/转码未完成，等待其可点击（首次提示一次，避免刷屏）
		if found == "disabled" && !disabledNoticed {
			logger.Print("WX6", "发表按钮当前为禁用态（视频可能仍在上传/转码），等待其可点击")
			disabledNoticed = true
		}
		// 长时间找不到可点击按钮：定期输出候选诊断，便于从日志定位（文案不匹配/按钮未渲染等）
		if time.Since(lastDiag) >= 30*time.Second {
			logger.Print("WX6", "按钮候选诊断: "+publishBtnDiagnostics(fs))
			lastDiag = time.Now()
		}
		time.Sleep(800 * time.Millisecond)
	}
	logger.Print("WX6", "超时未找到可点击的发表按钮，最终诊断: "+publishBtnDiagnostics(fs))
	return errors.New("WX6 cannot find publish button on weixin channels page")
}

// publishBtnDiagnostics 枚举作用域内含"发表/发布"文案的按钮候选（标签|文案|禁用|可见），供找不到按钮时定位原因。
func publishBtnDiagnostics(fs frameScope) string {
	var info string
	_ = fs.evaluate(&info, `(function(){
		var els = document.querySelectorAll('button, div[role="button"], span[role="button"], a');
		var out = [];
		for (var i = 0; i < els.length; i++) {
			var el = els[i];
			var t = ((el.innerText || '') + '').replace(/\s+/g, ' ').trim();
			if (!t || t.length > 12 || t.indexOf('发表') < 0) continue;
			var r = el.getBoundingClientRect();
			var dis = el.disabled || el.getAttribute('aria-disabled') === 'true' || (((el.className || '') + '').indexOf('disabled') >= 0);
			out.push(el.tagName + '|' + t + '|dis=' + dis + '|vis=' + (r.width > 0 && r.height > 0));
			if (out.length >= 8) break;
		}
		return out.join(' ;; ');
	})()`, 5*time.Second)
	if info == "" {
		return "未找到任何含发表文案的按钮候选(子应用可能未渲染或文案不匹配)"
	}
	return info
}

// trustedClickPublishButton 对已标记的发表按钮做 CDP 真实鼠标事件点击（滚入视口后取中心坐标，
// 沙箱 iframe 已铺满视口左上角，内部坐标≈主视口坐标）。成功派发返回 true。
func trustedClickPublishButton(fs frameScope, logger *logx.Logger) bool {
	var coord string
	_ = fs.evaluate(&coord, `(function(){
		var el = document.querySelector('[__wx_pub_target]');
		if (!el) return '';
		try { el.scrollIntoView({block:'center', inline:'center'}); } catch(e){}
		var r = el.getBoundingClientRect();
		if (r.width <= 0 || r.height <= 0) return '';
		return (r.left + r.width / 2) + ' ' + (r.top + r.height / 2);
	})()`, 3*time.Second)
	parts := strings.Fields(coord)
	if len(parts) != 2 {
		return false
	}
	var cx, cy float64
	if _, err := fmt.Sscanf(parts[0]+" "+parts[1], "%f %f", &cx, &cy); err != nil || (cx <= 0 && cy <= 0) {
		return false
	}
	// 滚动完成后短暂等待布局稳定再取最终坐标（滚入视口后位置可能变化）
	time.Sleep(400 * time.Millisecond)
	_ = fs.evaluate(&coord, `(function(){
		var el = document.querySelector('[__wx_pub_target]');
		if (!el) return '';
		var r = el.getBoundingClientRect();
		if (r.width <= 0 || r.height <= 0) return '';
		return (r.left + r.width / 2) + ' ' + (r.top + r.height / 2);
	})()`, 3*time.Second)
	parts = strings.Fields(coord)
	if len(parts) == 2 {
		_, _ = fmt.Sscanf(parts[0]+" "+parts[1], "%f %f", &cx, &cy)
	}
	clickCtx, cancel := context.WithTimeout(fs.ctx, 5*time.Second)
	err := chromedp.Run(clickCtx,
		input.DispatchMouseEvent(input.MousePressed, cx, cy).WithButton(input.Left).WithClickCount(1),
		input.DispatchMouseEvent(input.MouseReleased, cx, cy).WithButton(input.Left).WithClickCount(1),
	)
	cancel()
	if err != nil {
		logger.Print("WX6", "可信点击发表按钮失败: "+err.Error())
		return false
	}
	logger.Print("WX6", fmt.Sprintf("已可信点击发表按钮 (坐标 %.0f,%.0f)", cx, cy))
	return true
}

func checkAnomalyContext(fs frameScope, originalErr error) error {
	if kw := checkAnomalyText(fs); kw != "" {
		return fmt.Errorf("WX3 weixin channels account anomaly (matched '%s'): account restricted interrupted the upload process", kw)
	}
	return originalErr
}

// checkAnomalyText 检测账号受限类专属文案，返回命中的关键词（无命中返回空）。
// 不使用泛化的"违规"等词，避免命中发表页上的合规提示文案。
// 在 iframe 作用域下同时检查 iframe 与主框架（风控弹窗可能覆盖全页渲染在主框架上）。
func checkAnomalyText(fs frameScope) string {
	js := `(function(){
		var body = document.body ? (document.body.innerText || "") : "";
		var kws = ["账号异常", "功能受限", "账号被封禁", "账号被限制", "暂时无法使用", "已被限制功能", "环境异常"];
		for (var i = 0; i < kws.length; i++) {
			if (body.indexOf(kws[i]) >= 0) return kws[i];
		}
		return "";
	})()`
	var matched string
	_ = fs.evaluate(&matched, js, 3*time.Second)
	if matched != "" {
		return matched
	}
	if fs.iframeSrc != "" {
		var mainMatched string
		mainCtx, cancel := context.WithTimeout(fs.ctx, 3*time.Second)
		defer cancel()
		_ = chromedp.Run(mainCtx, chromedp.Evaluate(js, &mainMatched))
		return mainMatched
	}
	return matched
}

func checkSomethingWentWrong(fs frameScope) bool {
	var hasError bool
	checkJs := `(function(){
		var body = document.body ? (document.body.innerText || "") : "";
		if(body.indexOf('页面出错') >= 0 || body.indexOf('加载失败') >= 0 || body.indexOf('请刷新') >= 0){
			return true;
		}
		return false;
	})()`
	_ = fs.evaluate(&hasError, checkJs, 5*time.Second)
	return hasError
}
