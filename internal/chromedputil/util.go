package chromedputil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// pageTargets 列出浏览器中所有的 page 标签页。
// ctx 必须是 chromedp.NewContext 创建的浏览器上下文(带有 Browser executor)。
func pageTargets(ctx context.Context) ([]*target.Info, error) {
	targets, err := chromedp.Targets(ctx)
	if err != nil {
		return nil, err
	}
	var pages []*target.Info
	for _, t := range targets {
		if t.Type == "page" && t.TargetID != "" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// browserExecutor 从 chromedp context 中取出 browser executor, 用于执行浏览器级 CDP 命令。
func browserExecutor(ctx context.Context) (cdp.Executor, error) {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Browser == nil {
		return nil, errors.New("chromedputil: context 缺少 browser executor(请传入 chromedp.NewContext 创建的上下文)")
	}
	return c.Browser, nil
}

// closePageTarget 通过 browser executor 关闭指定标签页。
func closePageTarget(ctx context.Context, exec cdp.Executor, id target.ID) error {
	closeCtx, cancelClose := context.WithTimeout(ctx, 3*time.Second)
	defer cancelClose()
	return target.CloseTarget(id).Do(cdp.WithExecutor(closeCtx, exec))
}

// CloseAllTabsThenBrowser 关闭浏览器中所有 page 标签页(供停止 profile 前清理用)。
// ctx 必须是 chromedp.NewContext 创建的浏览器上下文, 不能是 allocator 上下文。
func CloseAllTabsThenBrowser(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	pages, err := pageTargets(ctx)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		return nil
	}

	exec, err := browserExecutor(ctx)
	if err != nil {
		return err
	}

	for _, t := range pages {
		if err := closePageTarget(ctx, exec, t.TargetID); err != nil {
			return err
		}
	}
	return nil
}

// CleanExtraTabs 关闭多余的标签页，只保留第一个。
// ctx 必须是 chromedp.NewContext 创建的浏览器上下文, 不能是 allocator 上下文。
//
// ⚠️ 作为「任务里的第一个 chromedp 调用」，这里传的 ctx 必须是 chromedp 上下文本身，
// 绝不能是它派生的「单次操作超时」子上下文（context.WithTimeout(ctx, ...)）。
// chromedp 会把「首次 Allocate 所用的那个 ctx」与整条 CDP 连接绑定（见 chromedp/allocate.go）：
//
//	go func() { <-ctx.Done(); Cancel(ctx); cancel() /* 关闭 websocket */ }()
//
// 于是这个 ctx 一旦被 cancel —— 哪怕只是它派生出来的子 ctx 走过一次正常的单步超时 ——
// 就会升级成对整个上下文的 Cancel，后续所有 chromedp 调用立刻返回 "context canceled"，
// 且错误信息完全看不出真正原因（表现为任务刚起来就秒失败）。
// 正确姿势：首个 chromedp 调用直接传 chromedp 上下文本身；之后再套子 ctx 做单步超时。
//
// 返回的 error 语义很重要：本函数通常是任务里**第一个** chromedp.Run 调用。
// 一旦返回错误（尤其"获取标签页列表失败"，即连不上浏览器），说明 chromedp 的
// 「首次分配 Browser」已经失败 —— 而 chromedp 在失败前就已 close 掉内部的
// allocated channel，失败后又不记录 Browser（chromedp.go:299-305）。此时若继续调用
// 任何 chromedp.Run，会再次进入分配逻辑并对同一个 channel 二次 close，直接
// panic: close of closed channel 并终止整个进程。
// 因此调用方拿到 error 后必须立即 return，不得再执行任何 chromedp 操作。
func CleanExtraTabs(ctx context.Context, logger *logx.Logger, platformTag string) error {
	if ctx.Err() != nil {
		logger.Print(platformTag, "CleanExtraTabs: context 已失效: "+ctx.Err().Error())
		return ctx.Err()
	}

	pages, err := pageTargets(ctx)
	if err != nil {
		logger.Print(platformTag, "获取标签页列表失败: "+err.Error())
		return fmt.Errorf("连接浏览器失败: %w", err)
	}

	// 如果只有一个或没有标签页，无需清理
	if len(pages) <= 1 {
		return nil
	}

	logger.Print(platformTag, fmt.Sprintf("发现 %d 个标签页，清理多余的 %d 个", len(pages), len(pages)-1))

	exec, err := browserExecutor(ctx)
	if err != nil {
		logger.Print(platformTag, err.Error())
		return err
	}

	// 保留第一个，关闭其余的
	for i := 1; i < len(pages); i++ {
		if err := closePageTarget(ctx, exec, pages[i].TargetID); err != nil {
			logger.Print(platformTag, "关闭标签页失败: "+err.Error())
		}
	}

	logger.Print(platformTag, "已清理多余标签页，保留1个")
	return nil
}

// ProbeBrowserAlive 在**完全不触碰 chromedp 上下文**的前提下，探测浏览器调试端口是否可用。
//
// 为什么需要它：若浏览器已闪退/退出，chromedp 的首次分配会失败并留下"已 close 但未记录
// Browser"的不一致状态，后续任意 chromedp.Run 都会 panic（详见 CleanExtraTabs 注释）。
// 因此在创建 allocator 之前先做一次纯 HTTP 探测，连不上就直接返回，
// 从源头避免进入那条会 panic 的路径。
//
// websocketURL 形如 ws://127.0.0.1:54913/devtools/browser/<id>，
// 探测目标是同端口的 http://127.0.0.1:54913/json/version。
func ProbeBrowserAlive(ctx context.Context, websocketURL string, timeout time.Duration) error {
	hostPort := hostPortFromWebsocket(websocketURL)
	if hostPort == "" {
		return fmt.Errorf("无法从 websocket 地址解析调试端口: %q", websocketURL)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+hostPort+"/json/version", nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("浏览器调试端口不可达(%s): %w", hostPort, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("浏览器调试端口返回异常状态(%s): %d", hostPort, resp.StatusCode)
	}
	return nil
}

// hostPortFromWebsocket 从 websocket 地址中提取 "host:port"。
func hostPortFromWebsocket(websocketURL string) string {
	s := strings.TrimSpace(websocketURL)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if !strings.Contains(s, ":") {
		return ""
	}
	return s
}

// PageLoadTimeout 打开页面后的加载超时时间：页面在该时长内未完成加载则终止流程。
const PageLoadTimeout = 30 * time.Second

// ErrPageLoadTimeout 表示页面加载超时。
var ErrPageLoadTimeout = errors.New("页面加载超时")

// NavigateAndWaitBody 导航到指定 URL 并等待 body 就绪。
// 注意：这里直接用原 ctx 导航，不再套 context.WithTimeout 子上下文——chromedp 的
// Navigate 内部用 responseAction 监听 load/lifecycle 事件，子上下文的超时/取消会与
// 事件派发产生竞态，导致后续上传等操作被干扰。
func NavigateAndWaitBody(ctx context.Context, logger *logx.Logger, url, tag string) error {
	if err := chromedp.Run(ctx, chromedp.Navigate(url), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		return err
	}
	return nil
}

// CloseTabsAndStopProfile 关闭所有标签页, 并请求停止 Undetectable Profile。
// browserCtx 必须是 chromedp.NewContext 创建的浏览器上下文。
// websocketURL 为该 profile 的 CDP 地址，用于在 stop 接口失效时按调试端口定位浏览器进程做兜底清理。
func CloseTabsAndStopProfile(ctx context.Context, browserCtx context.Context, logger *logx.Logger,
	profileID, undetectableHost string, undetectablePort int, websocketURL string, platformTag string) {

	if browserCtx != nil {
		closeCtx, cancelClose := context.WithTimeout(browserCtx, 15*time.Second)
		if err := CloseAllTabsThenBrowser(closeCtx); err != nil {
			logger.Print(platformTag, "清理标签页遇到异常 (Best Effort): "+err.Error())
		} else {
			logger.Print(platformTag, "已清理冗余标签页")
		}
		cancelClose()
	}

	if profileID != "" && undetectableHost != "" && undetectablePort != 0 {
		stopCtx, cancelStop := context.WithTimeout(ctx, 30*time.Second)
		err := undetectable.NewClient(undetectableHost, undetectablePort).StopProfileBestEffort(stopCtx, profileID)
		cancelStop()

		if err != nil {
			logger.Print(platformTag, "请求停止 Undetectable Profile 失败: "+err.Error())
			// 兜底 1：通过 CDP 关闭浏览器本体
			CloseBrowserViaCDP(browserCtx, logger, platformTag)
			// 兜底 2：进程级清理。Undetectable 主程序崩溃/卡死时，stop 接口与 CDP 可能都失效，
			// 此时按该 profile 的调试端口特征直接结束浏览器进程树，避免进程永久残留。
			KillBrowserProcessesByHint(logger, RemoteDebugPortHint(websocketURL))
		} else {
			logger.Print(platformTag, "已成功请求停止 Undetectable Profile")
			time.Sleep(3 * time.Second)
			logger.Print(platformTag, "云端同步缓冲完成，配置安全关闭")
		}
	}
}

// ===== 进程级兜底清理 =====
// 背景：正常清理依赖 Undetectable 的 stop HTTP 接口 + CDP Browser.close 两条软通道。
// 一旦主程序崩溃/卡死，这两条通道都会失效，浏览器进程会变成孤儿进程永久残留（只能手工结束）。
// 因此提供一个"最后手段"：按进程名 + 命令行特征直接结束进程树。

// undetectedBrowserProcessName 兜底清理时匹配的浏览器进程名。
// 可用环境变量 UNDETECTED_BROWSER_PROC 覆盖（若实际进程名不是 Undetected.exe）。
var undetectedBrowserProcessName = "Undetected.exe"

func init() {
	if v := strings.TrimSpace(os.Getenv("UNDETECTED_BROWSER_PROC")); v != "" {
		undetectedBrowserProcessName = v
	}
}

// DebugPortFromWebsocket 从 CDP 的 websocket 地址解析调试端口。
// 例："ws://127.0.0.1:45678/devtools/browser/xxx" -> "45678"。
func DebugPortFromWebsocket(websocketURL string) string {
	s := strings.TrimSpace(websocketURL)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Port() != "" {
		return u.Port()
	}
	// 兜底：手工从 "host:port" 里截取端口
	hostPort := s
	if i := strings.Index(hostPort, "://"); i >= 0 {
		hostPort = hostPort[i+3:]
	}
	if i := strings.IndexAny(hostPort, "/?#"); i >= 0 {
		hostPort = hostPort[:i]
	}
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		return hostPort[i+1:]
	}
	return ""
}

// RemoteDebugPortHint 由 websocket 地址生成浏览器命令行匹配特征
// "--remote-debugging-port=<port>"；无法解析时返回空串。
func RemoteDebugPortHint(websocketURL string) string {
	if p := DebugPortFromWebsocket(websocketURL); p != "" {
		return "--remote-debugging-port=" + p
	}
	return ""
}

// RemoteDebugPortHintFromPort 用「裸调试端口」（Undetectable /list 返回的 debug_port 字段）
// 生成进程定位特征串。用于 websocket_link 已丢失、只能靠 debug_port 定位残留浏览器的场景。
func RemoteDebugPortHintFromPort(port string) string {
	p := strings.TrimSpace(port)
	if p == "" || p == "0" {
		return ""
	}
	return "--remote-debugging-port=" + p
}

// KillBrowserProcessesByHint 进程级兜底清理：按「浏览器进程名 + 命令行包含任一 hint」
// 定位并强制结束进程树（taskkill /F /T）。
//
// hints 必须是能**唯一标识某个 profile** 的特征串，例如：
//   - "--remote-debugging-port=45678"（该 profile 的调试端口）
//   - profile 的 user-data-dir 路径
//
// 传空串会被忽略；若最终没有任何有效特征，函数直接跳过并记日志 —— 宁可残留也不误杀，
// 因为盲目按进程名结束会杀掉其它并发任务正在使用的浏览器。
//
// 非 Windows 平台直接跳过（本项目部署在 Windows）。
func KillBrowserProcessesByHint(logger *logx.Logger, hints ...string) {
	if runtime.GOOS != "windows" {
		return
	}

	var conds []string
	for _, h := range hints {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		// PowerShell 单引号字符串内的单引号需转义为两个单引号
		esc := strings.ReplaceAll(h, "'", "''")
		conds = append(conds, fmt.Sprintf("($_.CommandLine -like '*%s*')", esc))
	}
	if len(conds) == 0 {
		logger.Print("KILL", "无有效定位特征，跳过进程级兜底清理（避免误杀其它浏览器）")
		return
	}

	script := fmt.Sprintf(`$ps = Get-CimInstance Win32_Process -Filter "Name='%s'" | Where-Object { %s }; `+
		`if ($ps) { $ps | ForEach-Object { & taskkill /F /T /PID $_.ProcessId } } else { 'NO_MATCH' }`,
		undetectedBrowserProcessName, strings.Join(conds, " -or "))

	killCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	out, err := exec.CommandContext(killCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		logger.Print("KILL", fmt.Sprintf("进程级兜底清理执行失败: %v output=%s", err, result))
		return
	}
	logger.Print("KILL", "进程级兜底清理结果: "+result)
}

// CloseBrowserViaCDP 通过 CDP 的 Browser.close 命令直接关闭浏览器本体。
// 作为 Undetectable stop 接口失败时的兜底：即使 HTTP stop 请求失效，也能当场关掉浏览器，
// 避免遗留一个空白标签页的浏览器、只能等 Undetectable 的空闲超时(约10分钟)才被关闭。
// browserCtx 必须是 chromedp.NewContext 创建的浏览器上下文。
func CloseBrowserViaCDP(browserCtx context.Context, logger *logx.Logger, tag string) {
	if browserCtx == nil || browserCtx.Err() != nil {
		return
	}
	exec, err := browserExecutor(browserCtx)
	if err != nil {
		logger.Print(tag, "CDP 关闭浏览器失败(无法获取 executor): "+err.Error())
		return
	}
	closeCtx, cancelClose := context.WithTimeout(browserCtx, 5*time.Second)
	defer cancelClose()
	if err := browser.Close().Do(cdp.WithExecutor(closeCtx, exec)); err != nil {
		logger.Print(tag, "CDP 关闭浏览器失败: "+err.Error())
		return
	}
	logger.Print(tag, "已通过 CDP 直接关闭浏览器本体")
}

// PageStallTimeout 页面停留超时默认值(30秒)
const PageStallTimeout = 30 * time.Second

// PageStallWatcher 页面停留监控器。
// 后台轮询当前页面 URL, 若同一个 URL 持续停留超过阈值则判定为卡在某个页面。
type PageStallWatcher struct {
	mu         sync.Mutex
	stalled    bool
	stalledURL string
}

// Stalled 是否检测到页面停留超时。
func (w *PageStallWatcher) Stalled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}

// URL 卡住的页面链接(仅在 Stalled 为 true 时有意义)。
func (w *PageStallWatcher) URL() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalledURL
}

func (w *PageStallWatcher) markStalled(url string) {
	w.mu.Lock()
	w.stalled = true
	w.stalledURL = url
	w.mu.Unlock()
}

// WatchPageStall 启动页面停留监控。
// parent 必须是 chromedp 浏览器上下文; 返回的 context 在检测到页面停留超时后会被取消。
// 调用方通过 watcher.Stalled()/URL() 获取是否卡住以及卡住的页面链接。
func WatchPageStall(parent context.Context, logger *logx.Logger, timeout time.Duration) (context.Context, context.CancelFunc, *PageStallWatcher) {
	if timeout <= 0 {
		timeout = PageStallTimeout
	}
	ctx, cancel := context.WithCancel(parent)
	w := &PageStallWatcher{}

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		var lastURL string
		var lastChange time.Time
		started := false

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var url string
				locCtx, locCancel := context.WithTimeout(ctx, 3*time.Second)
				err := chromedp.Run(locCtx, chromedp.Location(&url))
				locCancel()
				if err != nil {
					continue
				}
				if !started || url != lastURL {
					lastURL = url
					lastChange = time.Now()
					started = true
					continue
				}
				if time.Since(lastChange) >= timeout {
					w.markStalled(url)
					logger.Print("STALL", fmt.Sprintf("页面停留超过 %v, 判定为卡在页面: %s", timeout, url))
					cancel()
					return
				}
			}
		}
	}()

	return ctx, cancel, w
}
