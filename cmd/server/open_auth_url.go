package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"minimax_pro/internal/chromedputil"
	"minimax_pro/internal/logx"
)

// ===== postforme 授权页「打开 + 点击确认」能力 =====
//
// 背景：接入第三方发布平台 postforme 时，账号需要先授权。授权 URL 由 account_sys
// 调 postforme 接口拿到后，下发给机器端，让机器端用该账号绑定的指纹浏览器打开并
// 点击「Continue」按钮，点完即关浏览器 —— 授权是否最终成功不在此判断，统一由
// account_sys 轮询 postforme 确认。
//
// 本文件只做「打开 + 点击 + 关浏览器 + 反馈」四步，不涉及任何 postforme 接口的
// 业务字段，也不判断授权结果（机器端既无法、也无需判断）。
//
// ===== 实现原则：与采集/发文用**完全相同**的浏览器开关流程 =====
//
// 本文件不另起炉灶：不用 Target.createTarget 自建标签页、不做 WithTargetID 自建会话、
// 不自己写 Navigate、不搞独立的"会话重建"框架，而是照抄 main.go 的 FETCH / MSG / NURTURE
// 与 internal/platform/* 发布流程：
//
//	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(bgCtx, ws, chromedp.NoModifyURL)
//	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
//	    chromedp.WithLogf(...), chromedp.WithErrorf(...))   // BrowserOption，仅此处可传
//	chromedputil.CleanExtraTabs(browserCtx, logger, "AUTH") // 首次 chromedp 调用
//	tabCtx, cancelTab := chromedp.NewContext(browserCtx)    // 工作标签页，不可再传 Option
//	workCtx, cancelWork := context.WithTimeout(tabCtx, ...) // 任务级兜底超时（绝不在操作中途取消）
//	chromedputil.NavigateAndWaitBody(workCtx, logger, url, "AUTH") // 打开页面，用现成封装
//	... 在 workCtx 上 Evaluate 探测 / 点击 ...
//	stopProfileWithCleanup(...)                             // 统一关闭（stop 接口 + 双兜底）
//
// ===== 踩坑记录（都源于"绕过既有流程自己写"） =====
//
// 1) 在已有 browserCtx 上再 NewContext 时，**绝不能**传 chromedp.WithLogf / WithErrorf。
//
//	它们不是普通 ContextOption，而是 WithBrowserOption 的快捷方式（chromedp.go:518-543），
//	语义是"给即将**新分配**的浏览器设 logger"。Browser 已非 nil 时会直接 panic：
//
//	    WithBrowserOption can only be used when allocating a new browser
//
//	main.go:1277 早已记下这条（FETCH 流程踩过），本文件重新踩了一遍，直接导致任务
//	在"新建标签页"处秒 panic、且因未 defer 收尾而把浏览器留在打开状态。
//
// 2) **打开页面必须用 `chromedputil.NavigateAndWaitBody`（传 tabCtx 本身），
//    不要自己写 `WithTimeout(ctx, N秒)` 包 Navigate 再立刻 cancel。**
//
//	chromedp 的 Navigate 内部是 responseAction：它会 ListenTarget 监听
//	`page.EventLifecycleEvent` / `EventLoadEventFired` 等事件来判定加载完成。
//	包一层子 ctx 再在 Navigate 返回后立即 cancel，会与这些事件的派发产生竞态
//	（chromedp.go:673-692 明确处理了这个 select 竞态），后果是**后续所有页面级命令
//	（Runtime.evaluate / DOM.* / Page.*）全部挂到 context deadline exceeded**，
//	而 browser 级命令（Target.getTargets）照常 —— 日志表现极具迷惑性：
//	「授权页 target 明明在列表里，却什么都探测不到」。
//	本项目 11 个平台发布/私信流程统一走 NavigateAndWaitBody，本文件是唯一例外，
//	2026-09-24 为此付了一次失败。
//
// 3) 不要用 browser 级 Target.createTarget + ActivateTarget 自己造/前台化标签页。
//
//	ActivateTarget 会触发 Undetectable 的 und_activTabChanged，把 chromedp 的页面会话
//	搅乱。交给 chromedp 自己建标签页（NewContext 后首次 Run 会 CreateTarget + attach）即可。
//
// 4) 「关浏览器」必须是 defer 级别的收尾，不能只在成功/失败分支里手写。
//
//	任务里任何一处 panic（见 1）都会跳过手写的 cleanup，浏览器就此留在打开状态。
//	本文件把「清标签页 + 停 profile + 取消 CDP 上下文」整体包成一个幂等的 closeBrowser，
//	用 defer 注册，成功 / 失败 / panic 三条路径都会被关掉。
//
// 5) **跳转目标是无监听的 `127.0.0.1:9000` 时，`chromedp.Location` / `window.location.href`
//    读到的是 `chrome-error://chromewebdata/`，不是带 code 的原始 URL。**
//
//	Chrome 在 `net::ERR_CONNECTION_REFUSED` 后会渲染内置错误页，页面级 API 只能看到错误页
//	地址，**原始 URL 里的 `?code=...` 就丢了**。唯一可靠的读法是 browser 级的
//	`chromedp.Targets()` → page target 的 `URL` 字段，它在导航失败后**仍然保留原始请求 URL**。
//	2026-09-28 用 Edge（同 Chromium 内核）实测两条路径均如此：
//
//	    直接 Navigate 到 127.0.0.1:57321/callback?code=xxx
//	        Location="chrome-error://chromewebdata/"        ← 拿不到 code
//	        page target url="http://127.0.0.1:57321/callback?code=NF9kYW5SAMPLE123&state=abc123"  ← 有 code
//	    在 about:blank 上 JS `location.href=...` 触发跳转：结论完全一致
//
//	所以 X（Twitter）OAuth 认证（见下方 authModeLocalCode）**必须**用 Targets 轮询，
//	不能用 Location —— 契约文档里写的 Location 方案会直接失败。

// postformeAuthCallbackURL 检测到点击授权按钮后，回调 account_sys 告知「已点击完成授权」。
// 与 taskResultURL 同 host（47.89.235.227:3366）。account_sys 无论是否当场确认都回 success，
// 因此本回调 Best Effort、不重试。
const postformeAuthCallbackURL = "http://47.89.235.227:3366/api/v1/postforme/auth_callback"

// xAuthCallbackURL X（Twitter）OAuth 认证：截到授权码后回调 account_sys。
// code 是一次性的、account_sys 侧无轮询（丢了就只能人工重来），所以这个是**必达**回调，
// 失败要重试（见 xAuthCallbackRetries）。
const xAuthCallbackURL = "http://47.89.235.227:3366/api/v1/x_auth/auth_callback"

// 授权模式：同一个 POST /accounts/open_auth_url 接口承载两种流程。
const (
	// authModeClick postforme 模式：等「Continue」按钮 → JS 点击 → 等 URL 变化。
	authModeClick = "click"
	// authModeLocalCode X/OAuth 2.0 PKCE 模式：人工在浏览器里完成授权 → 等页面跳到本机
	// 回调地址（如 http://127.0.0.1:9000/callback?code=...）→ 从 URL 抠 code → 回调 account_sys。
	authModeLocalCode = "local_code"
)

const (
	// authWorkTimeout 「打开 + 点击 + 判定」整段工作的兜底超时。
	// ⚠️ 它只是兜底：绝不能在某个操作（尤其 Navigate）中途取消，否则会与 chromedp 的
	// 事件派发竞态，把后续所有页面命令搞成超时（见文件头踩坑 2）。
	authWorkTimeout = 10 * time.Minute
	// authButtonWait 等待「Continue」按钮出现的上限。
	authButtonWait = 60 * time.Second
	// authStepTimeout 单步页面命令（探测/取 URL）的超时。
	authStepTimeout = 15 * time.Second

	// xAuthDefaultWait X 认证等待用户完成授权的默认时长。
	// 授权过程要人工登录 + 点确认，比 postforme 的自动点击慢得多。
	xAuthDefaultWait = 10 * time.Minute
	// xAuthPollInterval 轮询"页面是否已跳到本机回调地址"的间隔。
	xAuthPollInterval = time.Second
	// xAuthCallbackRetries 授权码回调 account_sys 的重试次数（含首次）。
	xAuthCallbackRetries = 3
	// xAuthDefaultRedirectPrefix 兜底的本机回调前缀。
	// 正常路径由授权 URL 自身的 redirect_uri 参数解析得到，无需请求方额外传参。
	xAuthDefaultRedirectPrefix = "http://127.0.0.1:9000/callback"
)

// OpenAuthURLRequest 「打开授权 URL + 点击确认」任务请求体。
type OpenAuthURLRequest struct {
	ProfileName string `json:"profile_name"` // 指纹浏览器名（必填）
	URL         string `json:"url"`          // 授权 URL（必填）

	// 点击目标：两者至少给一个。优先 click_selector（CSS 选择器），找不到再用 click_text（按钮文本）兜底。
	ClickSelector string `json:"click_selector,omitempty"`
	ClickText     string `json:"click_text,omitempty"` // 缺省时按「Continue」文本匹配（默认目标 id="auth-btn"）

	// 点击后等待页面跳转的超时秒数（跳转 = 授权成功），默认 15；未跳转则判失败。
	RedirectWaitS int `json:"redirect_wait_seconds,omitempty"`

	// Mode 授权模式："click"（默认，postforme）/ "local_code"（X OAuth 认证）。
	// 不传时按 ref 前缀推断：XAuth: → local_code，其余 → click。显式传值优先级最高。
	Mode string `json:"mode,omitempty"`
	// RedirectPrefix local_code 模式下用于判定"已跳转到本机回调"的 URL 前缀。
	// 不传时自动从 url 里的 redirect_uri 参数解析，再兜底 xAuthDefaultRedirectPrefix。
	RedirectPrefix string `json:"redirect_prefix,omitempty"`

	Host             string `json:"host"`
	Port             int    `json:"port"`
	WaitSeconds      int    `json:"wait_seconds"`
	UndetectablePath string `json:"undetectable_path"`
	Async            bool   `json:"async"`
	Ref              string `json:"ref,omitempty"` // "PostformeAuth:<account_id>" 或 "XAuth:<account_id>"
	Batch            string `json:"batch,omitempty"`
}

// handleOpenAuthURL POST /accounts/open_auth_url —— 打开授权页 → 点确认 → 关浏览器 → 反馈。
func handleOpenAuthURL(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req OpenAuthURLRequest
		if _, err := decodeJSONBody(r, &req, 1<<20); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}
		if strings.TrimSpace(req.ProfileName) == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if strings.TrimSpace(req.URL) == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "url is required"})
			return
		}
		if req.Host == "" {
			req.Host = accountDefaultHost
		}
		if req.Port == 0 {
			req.Port = accountDefaultPort
		}
		if req.WaitSeconds <= 0 {
			req.WaitSeconds = accountDefaultWaitS
		}

		// 授权模式：显式 mode 优先，否则按 ref 前缀推断（XAuth: → local_code，其余 click）。
		mode := resolveAuthMode(req)

		logger.Print("AUTH", fmt.Sprintf("收到打开授权页请求: profile=%s mode=%s url=%s async=%v ref=%s",
			req.ProfileName, mode, safeSnippet(req.URL, 120), req.Async, req.Ref))

		// taskCtx 为任务根 context：异步时被替换为可取消 ctx，使 POST /tasks/clear 能中断。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string) {
			bgCtx := taskCtx

			// 1. 并发额度
			if err := acquireBrowserSlot(bgCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error()
			}
			defer releaseBrowserSlot()

			// 2. profile 锁（同一浏览器串行）。放锁必须晚于"关浏览器"，因此最早注册。
			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			// 3. 启动浏览器（带重试）
			res, err := startProfileByNameWithRetry(bgCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("AUTH", "启动Profile失败: "+err.Error())
				return "failed", "启动Profile失败: " + err.Error()
			}

			// 4. 收尾：关浏览器。浏览器一旦启动，成功 / 失败 / panic 都必须关掉它，
			//    所以这里用 defer 注册（而不是在各分支手写 cleanup），并用 sync.Once 保证幂等
			//    （成功路径会提前显式调用一次，好让回调晚于关浏览器）。
			var (
				browserCtx    context.Context
				cancelBrowser context.CancelFunc
				cancelAlloc   context.CancelFunc
				cancelTab     context.CancelFunc
				cdpReady      bool
			)
			var closeOnce sync.Once
			closeBrowser := func() {
				closeOnce.Do(func() {
					// 顺序与平台发布的 defer 顺序一致：先走既有 stop 流程（清标签页 + 停 profile
					// + CDP/进程级双兜底），再取消本地 CDP 上下文。
					if cdpReady {
						stopProfileWithCleanup(bgCtx, logger, browserCtx, res.Host, res.Port, res.ProfileID, res.Info.WebsocketLink)
					} else {
						// CDP 不可用（未建立 / 首次分配失败）：只走 Undetectable stop 接口。
						// 此处若还传 browserCtx，会在 chromedp 的不一致状态上再调 CDP，
						// 触发 panic(close of closed channel) 把整个进程带走 —— 必须传 nil。
						stopProfileWithCleanup(bgCtx, logger, nil, res.Host, res.Port, res.ProfileID, res.Info.WebsocketLink)
					}
					// 此时浏览器通常已关闭，这两个取消动作只是回收本地资源，失败无害。
					if cancelTab != nil {
						cancelTab()
					}
					if cancelBrowser != nil {
						cancelBrowser()
					}
					if cancelAlloc != nil {
						cancelAlloc()
					}
				})
			}
			defer closeBrowser()

			// 5. 建立 CDP 远程连接（与 FETCH / MSG / NURTURE 完全一致）
			allocCtx, ca := chromedp.NewRemoteAllocator(bgCtx, res.Info.WebsocketLink, chromedp.NoModifyURL)
			cancelAlloc = ca

			bctx, cb := chromedp.NewContext(allocCtx,
				chromedp.WithLogf(func(string, ...interface{}) {}),
				chromedp.WithErrorf(func(string, ...interface{}) {}),
			)
			browserCtx, cancelBrowser = bctx, cb

			// 6. 首次 chromedp 调用：分配 Browser（并清理多余标签页）。
			// 契约：本调用返回错误后**绝不能再碰任何 chromedp** —— 首次分配失败会留下
			// 「allocated channel 已 close 但 Browser 未记录」的不一致状态，再调会
			// panic: close of closed channel 并终止整个进程（详见 chromedputil 注释）。
			// 注意这里传的必须是 browserCtx 本身，不能是它的超时子 ctx（否则会绑定成
			// "首次分配所用 ctx"，子 ctx 一取消整条连接就废）。
			if err := chromedputil.CleanExtraTabs(browserCtx, logger, "AUTH"); err != nil {
				logger.Print("AUTH", "连接浏览器失败: "+err.Error())
				return "failed", "连接浏览器失败: " + err.Error()
			}
			cdpReady = true

			// 7. 工作标签页：与 FETCH 建 tab 的方式一致。
			// ⚠️ 此处**不能**传 chromedp.WithLogf / WithErrorf（见文件头踩坑 1）。
			tabCtx, ct := chromedp.NewContext(browserCtx)
			cancelTab = ct

			// 8. 任务级工作上下文：仅作兜底超时，**绝不在操作中途取消**。
			// 之所以不直接用 tabCtx：tabCtx 无超时，一旦页面永久不 ready 会卡住任务；
			// 而套子 ctx 又必须保证不会与 chromedp 的事件派发竞态（见文件头踩坑 2），
			// 所以这里给足兜底时长，只在极端情况下生效。
			// local_code 模式要等人工完成授权，兜底时长必须覆盖等待时长，否则会先被这层掐断。
			workTimeout := authWorkTimeout
			if mode == authModeLocalCode {
				if d := xAuthWaitDuration(req) + 2*time.Minute; d > workTimeout {
					workTimeout = d
				}
			}
			workCtx, cancelWork := context.WithTimeout(tabCtx, workTimeout)
			defer cancelWork()

			// 9. 打开授权页 —— 必须复用既有封装 NavigateAndWaitBody：
			// 它内部直接用传入的 ctx 跑 Navigate+WaitReady，不套子 ctx、不在中途 cancel。
			if err := chromedputil.NavigateAndWaitBody(workCtx, logger, req.URL, "AUTH"); err != nil {
				logger.Print("AUTH", "打开授权页失败: "+err.Error())
				return "failed", "打开授权页失败: " + err.Error()
			}
			logger.Print("AUTH", "已打开授权页: "+safeSnippet(currentPageURL(workCtx, 5*time.Second), 160))

			// ===== local_code 模式（X / Twitter OAuth 2.0 PKCE）=====
			// 与 postforme 的唯一区别：**不点任何按钮**，等人工在浏览器里完成授权后页面
			// 自己跳到本机回调地址，然后从该 URL 抠 code 回传 account_sys。
			//
			// ⚠️ 判定跳转必须走 browser 级 Targets（见文件头踩坑 5）：目标端口没有服务监听，
			// Chrome 会渲染 chrome-error://chromewebdata/ 错误页，Location 读不到原始 URL。
			if mode == authModeLocalCode {
				prefix := resolveRedirectPrefix(req)
				waitDur := xAuthWaitDuration(req)
				logger.Print("AUTH", fmt.Sprintf("X 认证：等待页面跳转到 %s（最长 %s，需人工完成授权）", prefix, waitDur))

				cb, err := waitLocalCallback(browserCtx, workCtx, logger, prefix, waitDur)
				if err != nil {
					logger.Print("AUTH", "等待授权码失败: "+err.Error())
					return "failed", "等待授权码失败: " + err.Error()
				}
				logger.Print("AUTH", fmt.Sprintf("已截获授权码: code=%s state=%s url=%s",
					maskSecret(cb.Code), safeSnippet(cb.State, 60), safeSnippet(cb.URL, 200)))

				// 先关浏览器，再回调（与 postforme 流程同序）
				closeBrowser()
				if err := callXAuthCallback(logger, req.Ref, cb); err != nil {
					logger.Print("AUTH_CB", "授权码回调失败: "+err.Error())
					return "failed", "已截获授权码但回调 account_sys 失败: " + err.Error()
				}
				return "success", "已获取授权码并回调 account_sys"
			}

			// 10. 等按钮 → 点击 → 等跳转（跳转 = 授权成功）
			if err := clickAuthButton(workCtx, logger, req); err != nil {
				logger.Print("AUTH", "点击授权按钮失败: "+err.Error())
				return "failed", "点击授权按钮失败: " + err.Error()
			}
			logger.Print("AUTH", "授权成功，页面已跳转")

			// 11. 先关闭浏览器（关闭本身也留出 postforme 处理授权的缓冲时间）
			closeBrowser()

			// 12. 关完再回调 account_sys 告知「已点击完成授权」
			callPostformeAuthCallback(logger, req.Ref)

			return "success", "授权成功，页面已跳转"
		}

		execute = guardVoid(logger, execute)

		if req.Async {
			taskID, tctx := registerTask("open_auth_url", req.ProfileName, req.Ref, req.Batch, 0, "", nil)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info := execute()
				finalizeAsyncTask(logger, taskID, "open_auth_url", req.ProfileName, req.Ref, status, info, nil)
			}()
			return
		}

		status, info := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"type": "success", "info": info})
	}
}

// authButtonJS 生成按钮探测/点击脚本：先按 CSS 选择器找，找不到再用按钮文本兜底。
func authButtonJS(selector, text string) (existsJS, clickJS string) {
	var candidates []string
	if sel := strings.TrimSpace(selector); sel != "" {
		candidates = append(candidates, fmt.Sprintf("document.querySelector(%q)", sel))
	}
	if t := strings.TrimSpace(text); t != "" {
		candidates = append(candidates, fmt.Sprintf(`(function(){
			const els = document.querySelectorAll('button, a, input[type=submit], [role=button]');
			for (const el of els) {
				const s = (el.innerText || el.value || el.textContent || '').trim();
				if (s && s.indexOf(%q) >= 0) return el;
			}
			return null;
		})()`, t))
	}
	if len(candidates) == 0 {
		return "false", "void 0"
	}

	lookup := "(" + strings.Join(candidates, " || ") + ")"
	return "!!" + lookup, "(function(){ const el = " + lookup + "; if (el) el.click(); })()"
}

// clickAuthButton 等待授权确认按钮出现 → 点击 → 等页面跳转（跳转 = 授权成功）。
//
// 默认目标：postforme 授权页的「Continue」按钮（id="auth-btn"，文本 Continue）。
// 三段式流程，其中「是否跳转」只看当前页面 URL 是否变化，不看任何业务字段。
//
// workCtx 是任务级工作上下文（tabCtx 的子 ctx），期间**绝不取消** —— 见文件头踩坑 2。
func clickAuthButton(workCtx context.Context, logger *logx.Logger, req OpenAuthURLRequest) error {
	selector := strings.TrimSpace(req.ClickSelector)
	text := strings.TrimSpace(req.ClickText)
	if selector == "" && text == "" {
		selector = "#auth-btn"
		text = "Continue"
	}
	existsJS, clickJS := authButtonJS(selector, text)

	// 阶段 1：等按钮出现（轮询页面，最长 authButtonWait）
	var lastErr error
	found := false
	deadline := time.Now().Add(authButtonWait)
	for time.Now().Before(deadline) {
		var exists bool
		if err := evalBool(workCtx, existsJS, &exists, authStepTimeout); err != nil {
			lastErr = err
		} else if exists {
			found = true
			break
		}
		if cerr := workCtx.Err(); cerr != nil {
			return fmt.Errorf("等待授权按钮期间任务被中断(%v)；%s", cerr, describePage(workCtx, lastErr))
		}
		time.Sleep(time.Second)
	}
	if !found {
		return fmt.Errorf("超时未找到授权按钮(selector=%s text=%s)；%s", selector, text, describePage(workCtx, lastErr))
	}
	logger.Print("AUTH", "已找到授权按钮，准备点击")

	// 阶段 2：点击前一刻取基准 URL（授权页在等待期间自己跳转是常事，取早了会误判）
	baseURL := currentPageURL(workCtx, 5*time.Second)

	// 点击会触发导航，命令本身可能因页面卸载而超时 —— 忽略错误、不重试（避免重复授权）。
	// 这里套子 ctx 是安全的：Evaluate 不是 responseAction，不会像 Navigate 那样与
	// load/lifecycle 事件派发竞态（见文件头踩坑 2）；套它只是防止命令悬挂太久。
	clickCtx, cancelClick := context.WithTimeout(workCtx, 5*time.Second)
	_ = chromedp.Run(clickCtx, chromedp.Evaluate(clickJS, nil))
	cancelClick()

	// 阶段 3：等跳转（URL 变化 = 授权成功）
	redirectWait := req.RedirectWaitS
	if redirectWait <= 0 {
		redirectWait = 15
	}
	deadline = time.Now().Add(time.Duration(redirectWait) * time.Second)
	for time.Now().Before(deadline) {
		curURL := currentPageURL(workCtx, 5*time.Second)
		if curURL != "" && curURL != baseURL {
			logger.Print("AUTH", fmt.Sprintf("页面已跳转，判定授权成功: %s -> %s",
				safeSnippet(baseURL, 100), safeSnippet(curURL, 100)))
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("点击后 %d 秒未跳转（基准 URL=%s）；%s",
		redirectWait, safeSnippet(baseURL, 100), describePage(workCtx, nil))
}

// evalBool 在页面上下文中求值一个返回布尔的 JS 表达式。
func evalBool(ctx context.Context, js string, out interface{}, timeout time.Duration) error {
	evalCtx, cancelEval := context.WithTimeout(ctx, timeout)
	defer cancelEval()
	return chromedp.Run(evalCtx, chromedp.Evaluate(js, out))
}

// currentPageURL 取当前页面 URL（页面级，与 chromedputil.WatchPageStall 同款）。
// 取不到（超时/会话不可用）时返回空串。
func currentPageURL(ctx context.Context, timeout time.Duration) string {
	locCtx, cancelLoc := context.WithTimeout(ctx, timeout)
	defer cancelLoc()
	var pageURL string
	if err := chromedp.Run(locCtx, chromedp.Location(&pageURL)); err != nil {
		return ""
	}
	return pageURL
}

// describePage 收集失败现场的页面信息（URL / 标题 / 全部 target 列表），便于定位。
// lastErr 为最近一次真实错误（可为 nil）。
func describePage(ctx context.Context, lastErr error) string {
	var title string
	titleCtx, cancelTitle := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(titleCtx, chromedp.Title(&title))
	cancelTitle()

	var descs []string
	if targets, err := chromedp.Targets(ctx); err == nil {
		for _, t := range targets {
			descs = append(descs, fmt.Sprintf("%s=%q", t.Type, safeSnippet(t.URL, 160)))
		}
	} else {
		descs = append(descs, "列举失败: "+err.Error())
	}

	return fmt.Sprintf("当前页 url=%q title=%q targets=[%s] lastErr=%v",
		safeSnippet(currentPageURL(ctx, 5*time.Second), 200), safeSnippet(title, 120),
		strings.Join(descs, ", "), lastErr)
}

// callPostformeAuthCallback 检测到点击后，回调 account_sys 告知「该账号已点击完成授权」。
// account_id 从 ref="PostformeAuth:<account_id>" 解析；social_account_id 留空（由 account_sys 反查）。
// Best Effort：account_sys 总是回 success，本函数不重试，失败只打日志。
func callPostformeAuthCallback(logger *logx.Logger, ref string) {
	accountID := ""
	if strings.HasPrefix(ref, "PostformeAuth:") {
		accountID = strings.TrimPrefix(ref, "PostformeAuth:")
	}

	body, err := json.Marshal(map[string]string{
		"account_id":        accountID,
		"social_account_id": "",
	})
	if err != nil {
		logger.Print("AUTH_CB", "构造授权回调体失败: "+err.Error())
		return
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, postformeAuthCallbackURL, bytes.NewReader(body))
	if err != nil {
		logger.Print("AUTH_CB", "构建授权回调失败: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", accountCheckUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Print("AUTH_CB", "授权回调失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	logger.Print("AUTH_CB", fmt.Sprintf("授权点击已回调 account_sys: account_id=%s status=%d body=%s",
		accountID, resp.StatusCode, strings.TrimSpace(string(raw))))
}

// ===== X（Twitter）OAuth 2.0 认证：本机回调截码 =====
//
// 与 postforme 模式的差异：
//   - postforme：机器端**替用户点**「Continue」，点完立刻算数；
//   - X：机器端**不碰页面**，等人工登录 + 点「Authorize app」，X 把浏览器跳转到
//     redirect_uri（如 http://127.0.0.1:9000/callback?code=...&state=...），机器端从 URL 抠 code。
//
// 注意：那个回调地址上**没有服务监听**，Chrome 会显示"无法访问此网站"错误页 —— 这正常，
// 不影响取参（原始 URL 保留在 browser 级 target 信息里，见文件头踩坑 5）。

// resolveAuthMode 决定授权流程模式：显式 mode 优先，否则按 ref 前缀推断。
// 前缀推断是为了让 account_sys 侧只用 ref 就能切换流程（契约约定 XAuth: vs PostformeAuth:）。
func resolveAuthMode(req OpenAuthURLRequest) string {
	if m := strings.TrimSpace(req.Mode); m != "" {
		return m
	}
	if strings.HasPrefix(strings.TrimSpace(req.Ref), "XAuth:") {
		return authModeLocalCode
	}
	return authModeClick
}

// xAuthWaitDuration 等待用户完成授权的时长：取 max(请求值, xAuthDefaultWait)。
// 之所以不用请求值直接兜底：请求缺省时 wait_seconds 会被填成 accountDefaultWaitS
// （45 秒，语义是"等 profile 启动"），对人工授权远远不够，会把正常授权掐断。
func xAuthWaitDuration(req OpenAuthURLRequest) time.Duration {
	d := xAuthDefaultWait
	if s := time.Duration(req.WaitSeconds) * time.Second; s > d {
		d = s
	}
	return d
}

// resolveRedirectPrefix 解析"本机回调地址"前缀，优先级：
// 请求显式传入 → 授权 URL 自身的 redirect_uri 参数（自动 URL 解码）→ 兜底默认值。
// 走 redirect_uri 的好处：account_sys 不用额外传参，端口改了也自动跟随。
func resolveRedirectPrefix(req OpenAuthURLRequest) string {
	if p := strings.TrimSpace(req.RedirectPrefix); p != "" {
		return p
	}
	if u, err := url.Parse(strings.TrimSpace(req.URL)); err == nil {
		if ru := strings.TrimSpace(u.Query().Get("redirect_uri")); ru != "" {
			return ru
		}
	}
	return xAuthDefaultRedirectPrefix
}

// localCallback 从本机回调 URL 里抠出的授权信息。
type localCallback struct {
	Code  string
	State string
	URL   string
}

// waitLocalCallback 轮询"是否有 page 标签页停在 prefix 指向的回调地址"，命中且带 code 即返回。
//
// 扫**所有** page 标签页而不是只看当前工作标签页：授权中途 X 可能换标签页，
// 或用户手动操作导致跳转发生在别的标签页上，扫全部更稳。
func waitLocalCallback(browserCtx, workCtx context.Context, logger *logx.Logger, prefix string, wait time.Duration) (localCallback, error) {
	deadline := time.Now().Add(wait)
	lastSeen := ""
	for {
		if raw := findRedirectURL(browserCtx, prefix); raw != "" {
			if raw != lastSeen {
				lastSeen = raw
				logger.Print("AUTH", "检测到跳转到本机回调地址: "+safeSnippet(raw, 200))
			}
			cb, ok, err := parseLocalCallback(raw)
			if err != nil {
				return localCallback{URL: raw}, err
			}
			if ok {
				return cb, nil
			}
			// 命中回调地址但 query 里还没 code：导航中间态，继续等
		}

		if err := workCtx.Err(); err != nil {
			return localCallback{}, fmt.Errorf("等待授权期间任务被中断(%v)；%s", err, describePage(workCtx, nil))
		}
		if !time.Now().Before(deadline) {
			return localCallback{}, fmt.Errorf("等待 %s 仍未检测到授权跳转（用户未完成授权或已中断）；%s",
				wait, describePage(workCtx, nil))
		}
		time.Sleep(xAuthPollInterval)
	}
}

// findRedirectURL 在所有 page 标签页里找 URL 命中本机回调前缀的那个，返回其 URL；没有则空串。
//
// 为什么不用 chromedp.Location：目标端口无服务时 Chrome 渲染 chrome-error://chromewebdata/，
// 页面级 API 只能看到错误页地址，`?code=` 就丢了；而 browser 级 Targets 里 page target 的
// URL 仍保留原始请求 URL —— 2026-09-28 实测确认（见文件头踩坑 5）。
func findRedirectURL(browserCtx context.Context, prefix string) string {
	ctx, cancel := context.WithTimeout(browserCtx, 5*time.Second)
	defer cancel()
	targets, err := chromedp.Targets(ctx)
	if err != nil {
		return ""
	}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if matchRedirectURL(t.URL, prefix) {
			return t.URL
		}
	}
	return ""
}

// matchRedirectURL 判断 u 是否落在 prefix 指向的回调地址上。
// 先字面前缀匹配；再退一步做 scheme/host/path 语义匹配，吸收大小写、尾斜杠等细微差异
// （X 回跳时可能对 URL 做规范化）。
func matchRedirectURL(u, prefix string) bool {
	if u == "" || prefix == "" {
		return false
	}
	if strings.HasPrefix(u, prefix) {
		return true
	}
	pu, err1 := url.Parse(u)
	pp, err2 := url.Parse(prefix)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(pu.Scheme, pp.Scheme) &&
		strings.EqualFold(pu.Host, pp.Host) &&
		strings.TrimRight(pu.EscapedPath(), "/") == strings.TrimRight(pp.EscapedPath(), "/")
}

// parseLocalCallback 解析回调 URL 的 query，返回 (授权信息, 是否已拿到 code, error)。
// error 非 nil 表示明确失败 —— 典型是 ?error=access_denied（用户点了拒绝）。
func parseLocalCallback(raw string) (localCallback, bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return localCallback{}, false, fmt.Errorf("解析回调 URL 失败: %w", err)
	}
	q := u.Query()
	cb := localCallback{Code: q.Get("code"), State: q.Get("state"), URL: raw}
	if cb.Code != "" {
		return cb, true, nil
	}
	if e := strings.TrimSpace(q.Get("error")); e != "" {
		if desc := strings.TrimSpace(q.Get("error_description")); desc != "" {
			return cb, false, fmt.Errorf("授权被拒绝或失败: %s (%s)", e, desc)
		}
		return cb, false, fmt.Errorf("授权被拒绝或失败: %s", e)
	}
	return cb, false, nil
}

// maskSecret 日志用：授权码只留首尾各 4 位，避免一次性凭证明文进日志。
func maskSecret(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

// callXAuthCallback 把抠到的授权码回调 account_sys（POST /api/v1/x_auth/auth_callback）。
// account_id 从 ref="XAuth:<account_id>" 解析；state 可选（抠不到就不传）。
//
// 与 postforme 回调不同，这是**必达**回调：code 一次性、account_sys 侧无轮询，
// 回调丢了该账号会一直卡"认证中"、只能人工重来 —— 所以失败要重试（退避 1s / 2s）。
func callXAuthCallback(logger *logx.Logger, ref string, cb localCallback) error {
	accountID := ""
	if strings.HasPrefix(ref, "XAuth:") {
		accountID = strings.TrimSpace(strings.TrimPrefix(ref, "XAuth:"))
	}

	payload := map[string]string{"account_id": accountID, "code": cb.Code}
	if cb.State != "" {
		payload["state"] = cb.State
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("构造回调体失败: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= xAuthCallbackRetries; attempt++ {
		if lastErr = postXAuthCallback(logger, body, accountID, attempt); lastErr == nil {
			return nil
		}
		if attempt < xAuthCallbackRetries {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return fmt.Errorf("重试 %d 次仍失败: %w", xAuthCallbackRetries, lastErr)
}

// postXAuthCallback 单次回调，2xx 视为成功。
func postXAuthCallback(logger *logx.Logger, body []byte, accountID string, attempt int) error {
	reqCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, xAuthCallbackURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", accountCheckUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Print("AUTH_CB", fmt.Sprintf("X 认证回调失败(第%d次): %v", attempt, err))
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	logger.Print("AUTH_CB", fmt.Sprintf("X 认证回调(第%d次): account_id=%s status=%d body=%s",
		attempt, accountID, resp.StatusCode, safeSnippet(strings.TrimSpace(string(raw)), 300)))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("account_sys 返回 %d: %s", resp.StatusCode,
			safeSnippet(strings.TrimSpace(string(raw)), 200))
	}
	return nil
}
