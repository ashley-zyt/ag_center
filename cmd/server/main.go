package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"minimax_pro/internal/auth"
	"minimax_pro/internal/chromedputil"
	chrome "minimax_pro/internal/clock"
	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/facebook"
	"minimax_pro/internal/platform/instagram"
	"minimax_pro/internal/platform/message"
	"minimax_pro/internal/platform/nurture"
	"minimax_pro/internal/platform/scraper"
	"minimax_pro/internal/platform/tiktok"
	"minimax_pro/internal/platform/twitter"
	"minimax_pro/internal/platform/youtube"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/chromedp"
)

type StartProfileRequest struct {
	ProfileName      string `json:"profile_name"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	WaitSeconds      int    `json:"wait_seconds"`
	UndetectablePath string `json:"undetectable_path"`
}

type StartProfileResponse struct {
	Type             string `json:"type"`
	ProfileID        string `json:"profile_id"`
	Status           string `json:"status"`
	DebugPort        string `json:"debug_port"`
	WebsocketLink    string `json:"websocket_link"`
	UndetectableHost string `json:"undetectable_host"`
	UndetectablePort int    `json:"undetectable_port"`
	ErrorInfo        string `json:"error_info,omitempty"`
}

type ErrorResponse struct {
	Type      string `json:"type"`
	ErrorInfo string `json:"error_info"`
}

type AccountCheckItem struct {
	ID          int    `json:"id"`
	Platform    string `json:"platform"`
	ProfileName string `json:"profile_name"`
}

type AccountCheckRequest struct {
	Host             string `json:"host"`
	Port             int    `json:"port"`
	WaitSeconds      int    `json:"wait_seconds"`
	UndetectablePath string `json:"undetectable_path"`
}

type AccountCheckResult struct {
	ID          int    `json:"id"`
	Platform    string `json:"platform"`
	ProfileName string `json:"profile_name"`
	Status      string `json:"status"`
	StatusDesp  string `json:"status_desp,omitempty"`
}

type AccountCheckResponse struct {
	Type    string               `json:"type"`
	Results []AccountCheckResult `json:"results"`
}

const (
	accountListURL      = "http://47.89.235.227:3366/api/v1/check/accounts"
	accountUpdateURL    = "http://47.89.235.227:3366/api/v1/check/update_account_status"
	accountCheckUA      = "Apifox/1.0.0 (https://apifox.com)"
	accountDefaultHost  = "127.0.0.1"
	accountDefaultPort  = 25325
	accountDefaultWaitS = 45

	// accountPostsUpdateURL is the endpoint that receives the scraped posts
	// for a single account. The exact URL will be provided later; it can be
	// overridden per-request via the FetchPostsRequest.UpdateAPIURL field or
	// via the POSTS_UPDATE_API_URL environment variable. When empty, the
	// update call is skipped and only logged.
	accountPostsUpdateURL = "http://47.89.235.227:3366/api/v1/post_stats"

	// accountStatsUpdateURL is the endpoint that receives batch account
	// statistics (followers, total posts) after post scraping completes.
	// Can be overridden via ACCOUNT_STATS_UPDATE_API_URL environment variable.
	accountStatsUpdateURL = "http://47.89.235.227:3366/api/v1/account_stats/batch_update"
)

// publishTimeout 发布等长流程使用独立 background context 时的超时上限，
// 避免因客户端/nginx 超时断开导致 r.Context() 被取消而中断发布与收尾。
const publishTimeout = 15 * time.Minute

// fetchStallTimeout 抓取流程的"页面停留超时"监控阈值。
// Facebook 抓取需在同一页面连续滚动多次(导航+等待+滚动约 40s+)，30s 会误判卡住，故放宽到 90s。
const fetchStallTimeout = 90 * time.Second

// profileOpMu 确保同一Profile同时只能执行一个浏览器操作(fetch/nurture), 避免并发操作同一浏览器导致标签页混乱、导航互相干扰
var (
	profileOpLocks   = make(map[string]*sync.Mutex)
	profileOpLocksMu sync.Mutex
)

// acquireProfileLock 获取指定Profile的操作锁, 返回释放函数
func acquireProfileLock(profileName string, logger *logx.Logger) func() {
	profileOpLocksMu.Lock()
	mu, ok := profileOpLocks[profileName]
	if !ok {
		mu = &sync.Mutex{}
		profileOpLocks[profileName] = mu
	}
	profileOpLocksMu.Unlock()

	logger.Print("LOCK", fmt.Sprintf("等待Profile锁: %s", profileName))
	mu.Lock()
	logger.Print("LOCK", fmt.Sprintf("已获取Profile锁: %s", profileName))
	return func() {
		mu.Unlock()
		logger.Print("LOCK", fmt.Sprintf("已释放Profile锁: %s", profileName))
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	// 先编码到buffer,验证UTF-8合法性后再写入,防止非法字节导致Rails端编码错误
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		// 编码失败时返回安全的错误JSON
		buf.Reset()
		buf.WriteString(`{"type":"error","error_info":"json encoding failed"}`)
	}
	// 验证并清理非法UTF-8字节
	data := buf.Bytes()
	if !utf8.Valid(data) {
		cleaned := strings.ToValidUTF8(string(data), "")
		data = []byte(cleaned)
	}
	_, _ = w.Write(data)
}

// sanitizeErr 清理错误信息中的非法UTF-8字符
func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	return scraper.SanitizeString(err.Error())
}

func downloadVideoFromOss(ctx context.Context, logger *logx.Logger, ossURL string) (string, error) {
	logger.Print("DL", "开始下载视频: "+ossURL)
	parsedURL, err := url.Parse(ossURL)
	if err != nil {
		return "", fmt.Errorf("invalid oss_url: %w", err)
	}

	resp, err := http.Get(ossURL)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	ext := filepath.Ext(parsedURL.Path)
	if ext == "" {
		ext = ".mp4"
	}

	videoDir := `C:\Users\Administrator\Desktop\videos\`
	if err := os.MkdirAll(videoDir, os.ModePerm); err != nil {
		return "", fmt.Errorf("create video directory failed: %w", err)
	}
	localPath := filepath.Join(videoDir, fmt.Sprintf("video_%d%s", time.Now().UnixNano(), ext))

	out, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create temp file failed: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		_ = os.Remove(localPath)
		return "", fmt.Errorf("save file failed: %w", err)
	}

	logger.Print("DL", "视频下载完成: "+localPath)
	return localPath, nil
}

func decodeJSONBody(r *http.Request, dst any, maxBytes int64) (string, error) {
	if r.Body == nil {
		return "", errors.New("empty body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(raw)) > maxBytes {
		return string(raw[:maxBytes]), fmt.Errorf("body too large: %d bytes", len(raw))
	}
	s := strings.TrimSpace(string(raw))
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if s == "" {
		return "", errors.New("empty body")
	}
	if err := json.Unmarshal([]byte(s), dst); err != nil {
		fixed := tryFixWindowsBackslashes(s)
		if fixed != s {
			if err2 := json.Unmarshal([]byte(fixed), dst); err2 == nil {
				return fixed, nil
			}
		}
		return s, err
	}
	return s, nil
}

func safeSnippet(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func tryFixWindowsBackslashes(s string) string {
	re := regexp.MustCompile(`(?s)("video_path"\s*:\s*")(.*?)(")`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		escaped := strings.ReplaceAll(sub[2], `\`, `\\`)
		return sub[1] + escaped + sub[3]
	})
}

func fetchAccountList(ctx context.Context) ([]AccountCheckItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, accountListURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", accountCheckUA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("account list http %d", resp.StatusCode)
	}
	var items []AccountCheckItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

func updateAccountStatus(ctx context.Context, id int, statusDesp string) error {
	q := url.Values{}
	q.Set("id", fmt.Sprintf("%d", id))
	q.Set("status_desp", statusDesp)
	u := accountUpdateURL + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", accountCheckUA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("update status http %d", resp.StatusCode)
	}
	return nil
}

type postsUpdatePayload struct {
	AccountID   int            `json:"account_id"`
	ProfileName string         `json:"profile_name"`
	Platform    string         `json:"platform"`
	SourceURL   string         `json:"source_url"`
	Posts       []scraper.Post `json:"posts"`
}

func resolvePostsUpdateURL(override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("POSTS_UPDATE_API_URL")); v != "" {
		return v
	}
	return accountPostsUpdateURL
}

func resolveAccountStatsUpdateURL() string {
	if v := strings.TrimSpace(os.Getenv("ACCOUNT_STATS_UPDATE_API_URL")); v != "" {
		return v
	}
	return accountStatsUpdateURL
}

type AccountStatParam struct {
	AccountID      int64 `json:"account_id"`
	TotalFollowers int   `json:"total_followers"`
	TotalPosts     int   `json:"total_posts"`
}

type accountStatsBatchPayload struct {
	Results []AccountStatParam `json:"results"`
}

func callBatchAccountStatsUpdateAPI(ctx context.Context, logger *logx.Logger, endpoint string, stats []AccountStatParam) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" || len(stats) == 0 {
		return nil
	}
	payload := accountStatsBatchPayload{Results: stats}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", accountCheckUA)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http status %d: %s", resp.StatusCode, safeSnippet(string(raw), 300))
	}

	logger.Print("ACCT_STATS", fmt.Sprintf("已成功批量推送账号统计数据 (共 %d 个账号)", len(stats)))
	return nil
}

func callPostsUpdateAPI(ctx context.Context, logger *logx.Logger, endpoint string, payload postsUpdatePayload) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", accountCheckUA)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http status %d: %s", resp.StatusCode, safeSnippet(string(raw), 300))
	}

	logger.Print("POSTS_UPD", fmt.Sprintf("已成功推送发文数据 (account_id=%d, platform=%s, posts=%d)", payload.AccountID, payload.Platform, len(payload.Posts)))
	return nil
}

func platformRule(platform string) (string, []string, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "youtube":
		return "https://www.youtube.com/", []string{"sign in", "sign in to like videos"}, nil
	case "twitter":
		return "https://x.com/home", []string{"already have an account", "create account", "sign in"}, nil
	case "tiktok":
		return "https://www.tiktok.com/tiktokstudio/upload", []string{"log in to tiktok", "sign up", "don't have an account", "don’t have an account"}, nil
	case "facebook":
		return "https://www.facebook.com/", []string{"confirm your identity", "confirm you're human to use your account", "log in", "sign up"}, nil
	default:
		return "", nil, fmt.Errorf("unknown platform: %s", platform)
	}
}

func pageContainsKeywords(ctx context.Context, keywords []string) (string, error) {
	if len(keywords) == 0 {
		return "", nil
	}
	normalized := make([]string, 0, len(keywords))
	for _, k := range keywords {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(k)))
	}
	kb, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	var matched string
	js := fmt.Sprintf(`(function(){
		var t=document.body?(document.body.innerText||""):"";
		t=t.toLowerCase();
		t=t.replace(/\u2019/g,"'").replace(/\u2018/g,"'");
		var kws=%s;
		for(var i=0;i<kws.length;i++){
			if(kws[i] && t.indexOf(kws[i])>=0){return kws[i];}
		}
		return "";
	})()`, string(kb))
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &matched)); err != nil {
		return "", err
	}
	return matched, nil
}

func checkAccountLogin(ctx context.Context, logger *logx.Logger, item AccountCheckItem, host string, port int, waitSeconds int, undetectablePath string) AccountCheckResult {
	res := AccountCheckResult{
		ID:          item.ID,
		Platform:    item.Platform,
		ProfileName: item.ProfileName,
		Status:      "abnormal",
	}

	pageURL, keywords, err := platformRule(item.Platform)
	if err != nil {
		res.StatusDesp = err.Error()
		return res
	}

	// 获取Profile操作锁(防止同一Profile的check/fetch/nurture并发执行)
	releaseLock := acquireProfileLock(item.ProfileName, logger)
	defer releaseLock()

	startRes, err := startProfileByName(ctx, logger, item.ProfileName, host, port, waitSeconds, undetectablePath)
	if err != nil {
		res.StatusDesp = err.Error()
		return res
	}

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, startRes.Info.WebsocketLink, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()

	// 清理多余标签页
	chromedputil.CleanExtraTabs(tabCtx, logger, "CHECK")

	defer stopProfileWithCleanup(context.Background(), logger, tabCtx, startRes.Host, startRes.Port, startRes.ProfileID, startRes.Info.WebsocketLink)

	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, 30*time.Second)
	defer cancelTimeout()

	if err := chromedp.Run(tabCtx, chromedp.Navigate(pageURL), chromedp.WaitVisible("body", chromedp.ByQuery)); err != nil {
		res.StatusDesp = "page load failed or timeout: " + err.Error()
		return res
	}

	var currentURL string
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`location.href`, &currentURL)); err != nil {
		res.StatusDesp = "get current url failed: " + err.Error()
		return res
	}
	if currentURL != "" && currentURL != pageURL {
		res.StatusDesp = "page redirected: " + currentURL
		return res
	}

	time.Sleep(15 * time.Second)

	matched, err := pageContainsKeywords(tabCtx, keywords)
	if err != nil {
		res.StatusDesp = err.Error()
		return res
	}
	if matched != "" {
		res.StatusDesp = "page contains keyword: " + matched
		return res
	}

	res.Status = "normal"
	return res
}

type startByNameResult struct {
	ProfileID string
	Info      undetectable.ProfileInfo
	Host      string
	Port      int
	Path      string
}

func startProfileByName(ctx context.Context, logger *logx.Logger, profileName string, host string, port int, waitSeconds int, undetectablePath string) (startByNameResult, error) {
	if profileName == "" {
		return startByNameResult{}, errors.New("profile_name is required")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 25325
	}
	if waitSeconds <= 0 {
		waitSeconds = 45
	}

	client, path, err := ensureAPIAndMaybeStart(ctx, logger, host, port, waitSeconds, undetectablePath)
	if err != nil {
		return startByNameResult{}, err
	}

	localCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds+20)*time.Second)
	defer cancel()

	logger.Print("2", "获取profile列表")
	profiles, err := client.ListProfiles(localCtx)
	if err != nil {
		return startByNameResult{}, err
	}
	logger.Print("2", "profile列表获取成功")

	logger.Print("3", "按名称查找profile")
	profileID, err := undetectable.FindProfileIDByName(profiles, profileName)
	if err != nil {
		return startByNameResult{}, err
	}
	logger.Print("3", "找到profile_id="+profileID)

	if info, ok := profiles[profileID]; ok && info.Status == "Started" {
		if info.WebsocketLink != "" {
			logger.Print("4", "profile已在运行中，跳过启动")
			return startByNameResult{ProfileID: profileID, Info: info, Host: host, Port: port, Path: path}, nil
		}
		// websocket_link 为空：说明这是残留/异常的浏览器实例（正常关闭不会残留），
		// 直接跳过启动会导致后续 PublishVideo 报 websocket_url is required。
		// 先停止残留实例，再走下方正常启动流程，重新获取有效的连接信息。
		logger.Print("4", "profile已在运行但 websocket_link 为空(疑似残留实例)，先停止再重新启动")
		_ = client.StopProfileBestEffort(localCtx, profileID)
		// 兜底：若 Undetectable 主程序曾崩溃/重启，它可能已「不认识」这个孤儿浏览器进程，
		// stop 接口会返回成功但进程仍在运行。此时按该 profile 的调试端口特征结束进程树，
		// 否则重新启动会复用脏实例，websocket_link 依旧为空。
		chromedputil.KillBrowserProcessesByHint(logger, chromedputil.RemoteDebugPortHintFromPort(info.DebugPort))
		time.Sleep(5 * time.Second)
	}

	logger.Print("4", "启动profile")
	startErr := client.StartProfileBestEffort(localCtx, profileID)

	// [合并保留本地优化] 💡 捕获锁被占用错误(Profile is locked / Unable to lock profile)并尝试释放重试
	if startErr != nil && isProfileLocked(startErr.Error()) {
		logger.Print("4", "检测到指纹浏览器已被占用/锁定，尝试强制释放并重试...")

		// 1. 调用停止接口尝试解锁
		_ = client.StopProfileBestEffort(localCtx, profileID)

		// 2. 必须等待几秒，给浏览器关闭进程和云端同步留出时间
		time.Sleep(10 * time.Second)

		// 3. 重新尝试启动
		logger.Print("4", "正在重新尝试启动 profile...")
		startErr = client.StartProfileBestEffort(localCtx, profileID)
	}

	if startErr == nil {
		logger.Print("4", "启动请求成功")
	} else {
		// 如果重试后依然失败，返回明确的错误提示
		if isProfileLocked(startErr.Error()) {
			return startByNameResult{}, fmt.Errorf("指纹浏览器已被占用/锁定，自动释放后重试依然失败")
		}
		logger.Print("4", "启动请求异常，尝试继续检测状态")
	}

	logger.Print("5", "等待profile进入 Started 状态")
	info, waitErr := undetectable.WaitProfileStarted(localCtx, client, profileID, time.Duration(waitSeconds)*time.Second)
	if waitErr != nil {
		if startErr != nil {
			// 假死信号：start 失败且 profile 最终也没起来。累计连续失败次数，
			// 达到阈值才判定主程序假死（单个失败可能偶发，避免误判触发重启）。
			if isUndetectableZombie(startErr.Error()) && noteUndetectableStartFailure() {
				logger.Print("4", fmt.Sprintf("连续 %d 个 profile 启动失败，判定 Undetectable 主程序假死/退出，触发熔断并尝试重启", undetectableZombieThreshold))
				markUndetectableBroken()
				if path != "" {
					go func() {
						restartCtx, cancelRestart := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancelRestart()
						_ = tryStartUndetectable(restartCtx, logger, path)
					}()
				}
			}
			return startByNameResult{}, startErr
		}
		return startByNameResult{}, waitErr
	}
	logger.Print("5", "已启动")
	// profile 最终成功启动，清零连续失败计数（主程序没假死）
	noteUndetectableStartSuccess()

	// 连接可用性预检：/list 报告 Started 且 websocket_link 非空，仍可能是刚刚崩掉的实例
	// （记录还没刷新、端口已经释放）。这里用**不经过 chromedp** 的纯 HTTP 探测再确认一次，
	// 从源头避免任务进入 chromedp 后踩到「首次分配失败 → 再次分配二次 close」的 panic 路径。
	if err := chromedputil.ProbeBrowserAlive(localCtx, info.WebsocketLink, 10*time.Second); err != nil {
		logger.Print("5", "浏览器连接预检失败(疑似刚崩溃/端口已释放): "+err.Error())
		// 实例已不可用：先停掉，避免残留占用；再由上层重试重新拉起。
		stopCtx, cancelPreStop := context.WithTimeout(context.Background(), 30*time.Second)
		_ = client.StopProfileBestEffort(stopCtx, profileID)
		cancelPreStop()
		return startByNameResult{}, fmt.Errorf("浏览器连接预检失败: %w", err)
	}
	logger.Print("5", "浏览器连接预检通过")

	return startByNameResult{ProfileID: profileID, Info: info, Host: host, Port: port, Path: path}, nil
}

// startProfileByNameWithRetry 启动浏览器并带重试：仅对"指纹浏览器相关"的临时性错误
// 重试（最多 browserStartRetryTimes 次，间隔 browserStartRetryInterval），其它错误直接返回。
func startProfileByNameWithRetry(ctx context.Context, logger *logx.Logger, profileName, host string, port, waitSeconds int, undetectablePath string) (startByNameResult, error) {
	var lastErr error
	for attempt := 0; attempt <= browserStartRetryTimes; attempt++ {
		res, err := startProfileByName(ctx, logger, profileName, host, port, waitSeconds, undetectablePath)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isRetryableBrowserError(err.Error()) || attempt == browserStartRetryTimes {
			break
		}
		logger.Print("RETRY", fmt.Sprintf("profile=%s 启动失败(第%d次)，%d秒后重试: %s",
			profileName, attempt+1, int(browserStartRetryInterval.Seconds()), err.Error()))
		select {
		case <-ctx.Done():
			return startByNameResult{}, ctx.Err()
		case <-time.After(browserStartRetryInterval):
		}
	}
	return startByNameResult{}, lastErr
}

// ===== 任务执行 panic 隔离 =====
//
// 背景：chromedp 在「首次分配 Browser 失败」后会留下不一致状态 —— 内部 allocated channel
// 已被 close，而 Browser 未记录（chromedp.go:299-305），此后任何 chromedp.Run 都会
// panic: close of closed channel。goroutine 中未被 recover 的 panic 会直接终止**整个进程**，
// 导致所有正在运行的任务一起中断、account_sys 永远收不到回调。
//
// 因此所有任务的 execute 闭包统一经下面三个包装函数执行：把任务内部的 panic 转成
// 「该任务失败」，把影响范围限制在单个任务内。
func guard[T any](logger *logx.Logger, fn func() (string, string, T)) func() (string, string, T) {
	return func() (status string, info string, out T) {
		defer func() {
			if r := recover(); r != nil {
				logger.Print("PANIC", fmt.Sprintf("任务执行发生 panic，已隔离(不影响其它任务): %v", r))
				status = "failed"
				info = fmt.Sprintf("任务异常中断(panic): %v", r)
				var zero T
				out = zero
			}
		}()
		return fn()
	}
}

// guardVoid 用于无附加返回值的任务（nurture）。
func guardVoid(logger *logx.Logger, fn func() (string, string)) func() (string, string) {
	return func() (status string, info string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Print("PANIC", fmt.Sprintf("任务执行发生 panic，已隔离(不影响其它任务): %v", r))
				status = "failed"
				info = fmt.Sprintf("任务异常中断(panic): %v", r)
			}
		}()
		return fn()
	}
}

// guardExtra 用于带两个附加返回值的任务（fetch：结果列表 + 错误信息）。
func guardExtra[T any](logger *logx.Logger, fn func() (string, string, T, string)) func() (string, string, T, string) {
	return func() (status string, info string, out T, extra string) {
		defer func() {
			if r := recover(); r != nil {
				logger.Print("PANIC", fmt.Sprintf("任务执行发生 panic，已隔离(不影响其它任务): %v", r))
				status = "failed"
				info = fmt.Sprintf("任务异常中断(panic): %v", r)
				var zero T
				out = zero
				extra = ""
			}
		}()
		return fn()
	}
}

func stopProfileWithCleanup(ctx context.Context, logger *logx.Logger, browserCtx context.Context, host string, port int, profileID string, websocketURL string) {
	if browserCtx != nil {
		closeCtx, cancelClose := context.WithTimeout(browserCtx, 10*time.Second)
		_ = chromedputil.CloseAllTabsThenBrowser(closeCtx)
		cancelClose()
	}
	// 停止 profile 需逐个尝试多个 API 端点，超时给足，避免端点响应稍慢就导致浏览器残留
	stopCtx, cancelStop := context.WithTimeout(ctx, 30*time.Second)
	err := undetectable.NewClient(host, port).StopProfileBestEffort(stopCtx, profileID)
	cancelStop()
	if err != nil {
		logger.Print("E", "停止 Profile 失败(浏览器可能未彻底关闭): "+err.Error())
		// 兜底 1：通过 CDP 关闭浏览器本体
		chromedputil.CloseBrowserViaCDP(browserCtx, logger, "STOP")
		// 兜底 2：进程级清理。Undetectable 主程序崩溃/卡死时 stop 接口与 CDP 可能都失效，
		// 此时按该 profile 的调试端口特征直接结束浏览器进程树，避免进程永久残留。
		chromedputil.KillBrowserProcessesByHint(logger, chromedputil.RemoteDebugPortHint(websocketURL))
	}
}

// [合并保留本地优化] isProfileLocked 判断启动/停止 profile 的错误是否为"锁被占用"类错误。
func isProfileLocked(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "profile is locked") ||
		strings.Contains(lower, "unable to lock profile") ||
		strings.Contains(lower, "failed to lock profile") ||
		strings.Contains(lower, "cannot lock profile")
}

// Undetectable 主程序拉起去重：多个并发任务都可能触发「探测失败→拉起」，
// 若不限制会在短时间内反复 exec 拉起多个 Undetectable 实例抢端口/锁，反而加剧主程序崩溃。
// 用冷却窗口保证一段时间内只真正拉起一次。
var (
	undetectableStartMu       sync.Mutex
	undetectableStartAt       time.Time
	undetectableStartCooldown = 15 * time.Second
)

// Undetectable 不可用熔断：主程序没启动/崩溃时，短时间内所有任务快速失败，
// 避免每个任务都走一遍「探测超时 + 重试 3 次」的昂贵流程、还误报成任务执行失败。
// 熔断期内任务入口直接返回「Undetectable 未启动」——该错误不在 isRetryableBrowserError
// 名单里，因此不会被重试；冷却期过后下一个任务重新探测，恢复即自动解除熔断。
var (
	undetectableBrokenMu    sync.Mutex
	undetectableBrokenUntil time.Time
)

// undetectableBreakCooldown 熔断持续时间：期间认为 Undetectable 不可用。
const undetectableBreakCooldown = 30 * time.Second

// isUndetectableBroken 是否处于熔断期（Undetectable 刚被确认不可用）。
func isUndetectableBroken() bool {
	undetectableBrokenMu.Lock()
	defer undetectableBrokenMu.Unlock()
	return time.Now().Before(undetectableBrokenUntil)
}

func markUndetectableBroken() {
	undetectableBrokenMu.Lock()
	undetectableBrokenUntil = time.Now().Add(undetectableBreakCooldown)
	undetectableBrokenMu.Unlock()
}

func clearUndetectableBroken() {
	undetectableBrokenMu.Lock()
	undetectableBrokenUntil = time.Time{}
	undetectableBrokenMu.Unlock()
}

// isUndetectableZombie 判断错误是否暗示 Undetectable 主程序已「假死/退出」：
// /status 探测通过了（才能走到启动 profile 这一步），但 profile 操作却报
// Invalid profile id / 超时，说明主程序的 profile 管理已失效——仅靠 /status 识别不出来。
// 这类错误应触发熔断 + 尝试重启主程序，而不是让任务重试 3 次后误报失败。
func isUndetectableZombie(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "invalid profile id") ||
		strings.Contains(lower, "context deadline exceeded")
}

// 连续假死信号失败计数：单个 profile start 失败可能是偶发，连续达到阈值才判定主程序假死，
// 避免偶发误判触发不必要的熔断/重启。
var (
	undetectableZombieMu     sync.Mutex
	undetectableZombieStreak int
)

// undetectableZombieThreshold 连续多少个 profile start 失败才判定主程序假死。
const undetectableZombieThreshold = 10

// noteUndetectableStartSuccess 记录一次 profile 最终启动成功，清零连续失败计数。
func noteUndetectableStartSuccess() {
	undetectableZombieMu.Lock()
	undetectableZombieStreak = 0
	undetectableZombieMu.Unlock()
}

// noteUndetectableStartFailure 记录一次假死信号失败，返回是否达到阈值（应判定假死）。
// 达到阈值时自动清零计数，避免重复触发。
func noteUndetectableStartFailure() bool {
	undetectableZombieMu.Lock()
	defer undetectableZombieMu.Unlock()
	undetectableZombieStreak++
	if undetectableZombieStreak >= undetectableZombieThreshold {
		undetectableZombieStreak = 0
		return true
	}
	return false
}

func resolveUndetectablePath(explicit string) string {
	// 对路径做 TrimSpace：环境变量/请求里常会带上行尾换行或首尾空格，
	// 若不剥掉，exec 会因文件名末尾的 \n 而报 "file does not exist"。
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("UNDETECTABLE_EXE")); p != "" {
		return p
	}
	return ""
}

// tryStartUndetectable 尝试拉起 Undetectable 主程序。
// 距上次启动不足冷却期时跳过（返回 nil，不重复拉起），让调用方继续等待 API 自愈；
// 真正执行启动失败时返回错误。
func tryStartUndetectable(ctx context.Context, logger *logx.Logger, path string) error {
	undetectableStartMu.Lock()
	defer undetectableStartMu.Unlock()

	if !undetectableStartAt.IsZero() && time.Since(undetectableStartAt) < undetectableStartCooldown {
		logger.Print("BOOT", "距上次启动 Undetectable 不足 15 秒，跳过重复启动，等待其自愈")
		return nil
	}
	undetectableStartAt = time.Now()

	logger.Print("BOOT", "尝试启动Undetectable: "+path)
	return undetectable.StartLocal(ctx, path)
}

func ensureAPIAndMaybeStart(ctx context.Context, logger *logx.Logger, host string, port int, waitSeconds int, explicitPath string) (*undetectable.Client, string, error) {
	// 熔断期：Undetectable 刚被确认不可用，直接快速失败，不再逐个探测/拉起。
	if isUndetectableBroken() {
		return nil, "", fmt.Errorf("Undetectable 未启动，任务暂停执行（熔断中，稍后自动恢复）")
	}

	logger.Print("1", "检查本地API服务")
	client := undetectable.NewClient(host, port)
	localCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds+20)*time.Second)
	defer cancel()

	err := client.Status(localCtx)
	if err == nil {
		logger.Print("1", "API服务正常")
		clearUndetectableBroken()
		return client, "", nil
	}
	// 记录失败原因，便于区分「主程序没起(connection refused)」「主程序卡死(timeout)」「状态异常(code≠0)」
	logger.Print("1", "API服务不可用，原因: "+err.Error())

	path := resolveUndetectablePath(explicitPath)
	if path == "" {
		markUndetectableBroken()
		return nil, "", fmt.Errorf("Undetectable 未启动（未配置 UNDETECTABLE_EXE 自动拉起），任务暂停执行")
	}

	if err := tryStartUndetectable(localCtx, logger, path); err != nil {
		markUndetectableBroken()
		return nil, "", fmt.Errorf("启动Undetectable失败: %w", err)
	}

	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for time.Now().Before(deadline) {
		if err := client.Status(localCtx); err == nil {
			logger.Print("1", "Undetectable已启动，API服务正常")
			clearUndetectableBroken()
			return client, path, nil
		}
		time.Sleep(2 * time.Second)
	}
	markUndetectableBroken()
	return nil, path, fmt.Errorf("已尝试启动Undetectable，但在超时时间内API仍不可用")
}

// ---------- CDP 日志过滤器 ----------

type cdpFilterLogger struct {
	logger *logx.Logger
}

func (l *cdpFilterLogger) Printf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	// 过滤 Undetectable 产生的已知 CDP 协议不兼容噪音
	if strings.Contains(msg, "could not unmarshal event") ||
		strings.Contains(msg, "unknown command or event") ||
		strings.Contains(msg, "Target.und_activTabChanged") ||
		strings.Contains(msg, "ClientNavigationReason") ||
		strings.Contains(msg, "initialFrameNavigation") {
		return
	}
	// 其余 CDP 日志暂不输出，避免干扰主流程日志
}

// ---------- /accounts/fetch_posts ----------

func fetchPostsByPlatform(ctx context.Context, logger *logx.Logger, platform string, req scraper.FetchRequest) (scraper.FetchResult, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "twitter", "x":
		return twitter.FetchPosts(ctx, logger, req)
	case "youtube", "yt":
		return youtube.FetchYoutubePosts(ctx, logger, req)
	case "instagram", "ig":
		return instagram.FetchInstagramPosts(ctx, logger, req)
	case "tiktok", "tt":
		return tiktok.FetchTikTokPosts(ctx, logger, req)
	case "facebook", "fb":
		return facebook.FetchFacebookPosts(ctx, logger, req)
	default:
		return scraper.FetchResult{}, fmt.Errorf("scraper: unsupported platform %q", platform)
	}
}

func nurtureByPlatform(ctx context.Context, logger *logx.Logger, platform string, req nurture.NurtureRequest) (nurture.NurtureResult, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "twitter", "x":
		return twitter.NurtureAccount(ctx, logger, req)
	case "youtube", "yt":
		return youtube.NurtureAccount(ctx, logger, req)
	case "instagram", "ig":
		return instagram.NurtureAccount(ctx, logger, req)
	case "tiktok", "tt":
		return tiktok.NurtureAccount(ctx, logger, req)
	case "facebook", "fb":
		return facebook.NurtureAccount(ctx, logger, req)
	default:
		return nurture.NurtureResult{Status: "failed", ErrorInfo: "unsupported platform: " + platform}, fmt.Errorf("nurture: unsupported platform %q", platform)
	}
}

// sendMessageByPlatform 分发批量私信任务到平台实现
func sendMessageByPlatform(ctx context.Context, logger *logx.Logger, platform string, tasks []message.SendTask) (message.SendResult, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "tiktok", "tt":
		return tiktok.SendTikTokMessage(ctx, logger, tasks)
	case "instagram", "ig":
		return instagram.SendInstagramMessage(ctx, logger, tasks)
	case "twitter", "x":
		return twitter.SendTwitterMessage(ctx, logger, tasks)
	case "facebook", "fb":
		return facebook.SendFacebookMessage(ctx, logger, tasks)
	case "youtube", "yt":
		// YouTube 已下线站内私信(Direct Messages)功能, 无可实现入口
		return message.SendResult{Status: "failed", ErrorInfo: "youtube 平台无站内私信功能(YouTube 已下线 Direct Messages)"}, fmt.Errorf("message send: youtube does not support direct messages")
	default:
		return message.SendResult{Status: "failed", ErrorInfo: "unsupported platform: " + platform}, fmt.Errorf("message send: unsupported platform %q", platform)
	}
}

// checkReplyByPlatform 分发"判断对方是否回复"任务到平台实现
// [合并保留本地优化] 保留了 Tiktok 和 Instagram 的调用
func checkReplyByPlatform(ctx context.Context, logger *logx.Logger, platform string, opts message.CheckReplyOptions) (message.CheckReplyResult, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "tiktok", "tt":
		return tiktok.CheckTikTokReply(ctx, logger, opts)
	case "twitter", "x":
		return twitter.CheckTwitterReply(ctx, logger, opts)
	case "instagram", "ig":
		return instagram.CheckInstagramReply(ctx, logger, opts)
	case "facebook", "fb":
		return facebook.CheckFacebookReply(ctx, logger, opts)
	default:
		return message.CheckReplyResult{Status: "failed", ErrorInfo: "unsupported platform: " + platform}, fmt.Errorf("message check_reply: unsupported platform %q", platform)
	}
}

// ─────────────────────────── 私信模块请求/响应结构 ───────────────────────────

// SendSingleMessageRequest POST /accounts/send_single_message 请求体(单条私信, 非批量)
type SendSingleMessageRequest struct {
	ProfileName      string `json:"profile_name"`           // 浏览器名称
	Platform         string `json:"platform"`               // 平台名
	TargetURL        string `json:"target_url"`             // 对方账号URL(统一参数, 所有平台均通过该链接定位对方)
	AccountName      string `json:"account_name,omitempty"` // 对方账号名(可选; 为空时各平台从 target_url 自行解析)
	MessageContent   string `json:"message_content"`        // 发消息的内容
	Passcode         string `json:"passcode,omitempty"`     // 平台密码验证(如 X 私信 Passcode, 默认1472)
	AccountID        int64  `json:"account_id"`             // 账号ID(由调用方传入, 用于追踪)
	Host             string `json:"host"`
	Port             int    `json:"port"`
	WaitSeconds      int    `json:"wait_seconds"`
	UndetectablePath string `json:"undetectable_path"`
	Async            bool   `json:"async"`           // true 时立即返回 task_id、后台执行、完成后回调
	Ref              string `json:"ref,omitempty"`   // 业务标识，透传：提交时传入，回调时原样带回
	Batch            string `json:"batch,omitempty"` // 批次标识，透传：同一批下发填同一个值，便于对账
}

// SendSingleMessageResponse POST /accounts/send_single_message 响应
type SendSingleMessageResponse struct {
	Type      string               `json:"type"`
	ProfileID string               `json:"profile_id"`
	AccountID int64                `json:"account_id"`
	Status    string               `json:"status"` // completed / failed / not_logged_in / error
	Result    *message.SendOutcome `json:"result,omitempty"`
	ErrorInfo string               `json:"error_info,omitempty"`
}

type CheckReplyRequest struct {
	ProfileName        string `json:"profile_name"`           // 浏览器名称
	Platform           string `json:"platform"`               // 平台名
	TargetURL          string `json:"target_url"`             // 对方账号URL(统一参数, 所有平台均通过该链接定位对方)
	AccountName        string `json:"account_name,omitempty"` // 对方账号名(可选; 为空时各平台从 target_url 自行解析)
	Passcode           string `json:"passcode,omitempty"`     // 平台密码验证(如 X 私信 Passcode, 默认1472)
	AccountID          int64  `json:"account_id"`             // 账号ID(由调用方传入, 用于追踪)
	SinceIncomingCount int    `json:"since_incoming_count"`   // 上次已看到的对方消息数(增量基线)
	Host               string `json:"host"`
	Port               int    `json:"port"`
	WaitSeconds        int    `json:"wait_seconds"`
	UndetectablePath   string `json:"undetectable_path"`
	Async              bool   `json:"async"`           // true 时立即返回 task_id、后台执行、完成后回调
	Ref                string `json:"ref,omitempty"`   // 业务标识，透传：提交时传入，回调时原样带回
	Batch              string `json:"batch,omitempty"` // 批次标识，透传：同一批下发填同一个值
}

// CheckReplyResponse POST /accounts/check_reply 响应
type CheckReplyResponse struct {
	Type        string            `json:"type"`
	ProfileID   string            `json:"profile_id"`
	AccountID   int64             `json:"account_id"`
	Status      string            `json:"status"`               // completed / failed / not_logged_in / error
	ReplyStatus string            `json:"reply_status"`         // replied=对方已回复 / awaiting_reply=等待对方回复
	HasReply    bool              `json:"has_reply"`            // 对方是否已回复
	ReplyCount  int               `json:"reply_count"`          // 本次返回的对方新回复条数
	Replies     []message.Message `json:"replies"`              // 对方发来的新回复(时间正序)
	CheckedAt   string            `json:"checked_at,omitempty"` // 本次查询的服务端时间(RFC3339)
	ErrorInfo   string            `json:"error_info,omitempty"`
}

type FetchPostsAccount struct {
	ID        int    `json:"id"`
	Platform  string `json:"platform"`
	SourceURL string `json:"source_url"`
}

type FetchPostsRequest struct {
	ID               int                 `json:"id"`
	ProfileName      string              `json:"profile_name"`
	ActiveAccounts   []FetchPostsAccount `json:"active_accounts"`
	Host             string              `json:"host"`
	Port             int                 `json:"port"`
	WaitSeconds      int                 `json:"wait_seconds"`
	UndetectablePath string              `json:"undetectable_path"`
	// UpdateAPIURL optionally overrides the per-account posts update endpoint.
	UpdateAPIURL string `json:"update_api_url,omitempty"`
	// Async true 时立即返回 task_id、后台执行、完成后回调。
	Async bool `json:"async"`
	// Ref 业务标识，透传：提交时传入，回调时原样带回（供 account_sys 精确关联任务）。
	Ref string `json:"ref,omitempty"`
	// Batch 批次标识，透传：同一批下发填同一个值，便于按批对账。
	Batch string `json:"batch,omitempty"`
}

type FetchPostsAccountResult struct {
	AccountID      int            `json:"account_id"`
	Platform       string         `json:"platform"`
	SourceURL      string         `json:"source_url"`
	Status         string         `json:"status"`
	Posts          []scraper.Post `json:"posts"`
	PostCount      int            `json:"post_count"`
	TotalFollowers int            `json:"total_followers"`
	TotalLikes     int            `json:"total_likes"`
	TotalPosts     int            `json:"total_posts"`
	StalledURL     string         `json:"stalled_url,omitempty"` // 页面停留超时卡住的页面链接
	ErrorInfo      string         `json:"error_info,omitempty"`
	UpdateSent     bool           `json:"update_sent"`
	UpdateError    string         `json:"update_error,omitempty"`
}

type FetchPostsResponse struct {
	Type      string                    `json:"type"`
	ProfileID string                    `json:"profile_id"`
	Results   []FetchPostsAccountResult `json:"results"`
	ErrorInfo string                    `json:"error_info,omitempty"`
}

// 🌟 1. 重新定义推送给 Rails 的单条数据 Payload，完美对齐 params 键名
type RailsPostParam struct {
	AccountID     int64  `json:"account_id"`
	PostDate      string `json:"post_date"` // 对应 params[:post_date]
	URL           string `json:"url"`       // 对应 params[:url]
	Title         string `json:"title"`
	LikesCount    int    `json:"likes_count"`
	SharesCount   int    `json:"shares_count"`
	CommentsCount int    `json:"comments_count"`
	ViewsCount    int    `json:"views_count"`
	DataUpdatedAt string `json:"data_updated_at"`
}

func handleFetchPosts(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req FetchPostsRequest
		_, err := decodeJSONBody(r, &req, 2<<20)
		if err != nil {
			logger.Print("E", "fetch_posts JSON解析失败: "+err.Error())
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}

		if req.ProfileName == "" || len(req.ActiveAccounts) == 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name and active_accounts are required"})
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

		updateEndpoint := resolvePostsUpdateURL(req.UpdateAPIURL)

		// execute 执行完整采集流程：并发额度 → profile锁 → 启动(带重试) → 遍历采集+上报 → 关闭释放。
		// 返回 (status, info, results, profileID)。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, []FetchPostsAccountResult, string) {
			bgCtx := taskCtx

			// 获取全局并发额度（排队等待）
			if err := acquireBrowserSlot(bgCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), nil, ""
			}
			defer releaseBrowserSlot()

			// 获取Profile操作锁(防止同一Profile的fetch和nurture并发执行导致浏览器混乱)
			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			// 1. 启动指纹浏览器环境
			logger.Print("FP1", fmt.Sprintf("启动Profile环境: %s (任务数: %d)", req.ProfileName, len(req.ActiveAccounts)))
			startRes, err := startProfileByNameWithRetry(bgCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", "启动Profile失败: "+err.Error())
				return "failed", err.Error(), nil, ""
			}

			// 建立 CDP 远程连接
			allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(bgCtx, startRes.Info.WebsocketLink, chromedp.NoModifyURL)
			defer cancelAlloc()

			// 🌟 双重静音保险：阻止 Chrome 底层未定义事件乱喷日志
			browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
				chromedp.WithLogf(func(string, ...interface{}) {}),
				chromedp.WithErrorf(func(string, ...interface{}) {}),
			)
			defer cancelBrowser()

			// 清理多余标签页
			chromedputil.CleanExtraTabs(browserCtx, logger, "FETCH")

			type tabInfo struct {
				ctx    context.Context
				cancel context.CancelFunc
			}
			openedTabs := make([]tabInfo, 0, len(req.ActiveAccounts))
			results := make([]FetchPostsAccountResult, 0, len(req.ActiveAccounts))
			statsEndpoint := resolveAccountStatsUpdateURL()
			if statsEndpoint != "" {
				logger.Print("FP3", fmt.Sprintf("账号统计更新API: %s (采集到粉丝数后立即上报)", statsEndpoint))
			}

			// 2. 串行遍历账号采集
			// 第一个账号复用browserCtx(已含错误抑制)，后续账号新建标签页(同样带错误抑制)，避免多余空白标签页
			for i, acc := range req.ActiveAccounts {
				res := FetchPostsAccountResult{
					AccountID: acc.ID,
					Platform:  acc.Platform,
					SourceURL: acc.SourceURL,
					Status:    "abnormal",
					Posts:     []scraper.Post{},
				}

				trimmedURL := strings.TrimSpace(acc.SourceURL)
				if acc.ID == 0 || strings.TrimSpace(acc.Platform) == "" {
					res.ErrorInfo = "账号参数不完整"
					results = append(results, res)
					continue
				}

				logger.Print("FP3", fmt.Sprintf("[%d/%d] 正在创建标签页任务 -> 平台: %s", i+1, len(req.ActiveAccounts), acc.Platform))

				var tabCtx context.Context
				var cancelTab context.CancelFunc
				if i == 0 {
					// 首个账号复用browserCtx，避免创建多余空白标签
					tabCtx = browserCtx
					cancelTab = cancelBrowser // 使用已有的cancel，不重复defer
				} else {
					// 注意: WithLogf/WithErrorf 是 BrowserOption(分配浏览器时用), 不是 ContextOption(创建tab时用)
					// 在已有的browserCtx上创建新tab时传入这些选项会导致 panic
					tabCtx, cancelTab = chromedp.NewContext(browserCtx)
				}
				openedTabs = append(openedTabs, tabInfo{ctx: tabCtx, cancel: cancelTab})

				fetchCtx, cancelFetch := context.WithTimeout(tabCtx, 20*time.Minute)
				stallCtx, cancelStall, watcher := chromedputil.WatchPageStall(fetchCtx, logger, fetchStallTimeout)
				fetchRes, fetchErr := fetchPostsByPlatform(stallCtx, logger, acc.Platform, scraper.FetchRequest{
					SourceURL:            trimmedURL,
					AccountID:            int64(acc.ID),
					AccountStatsEndpoint: statsEndpoint,
				})
				cancelStall()
				cancelFetch()

				if fetchErr != nil {
					if watcher.Stalled() {
						res.StalledURL = watcher.URL()
						res.ErrorInfo = "页面停留超时, 卡在页面: " + watcher.URL()
						logger.Print("FP3", fmt.Sprintf("[%d/%d] 页面停留超时, 卡在: %s", i+1, len(req.ActiveAccounts), watcher.URL()))
					} else {
						res.ErrorInfo = fetchErr.Error()
					}
				} else {
					res.Posts = fetchRes.Posts
					if res.Posts == nil {
						res.Posts = []scraper.Post{}
					}
					res.PostCount = len(res.Posts)
					res.TotalFollowers = fetchRes.TotalFollowers
					res.TotalLikes = fetchRes.TotalLikes
					res.TotalPosts = fetchRes.TotalPosts
					res.Status = "normal"
				}

				// 🌟 2. 核心调整：如果配置了接口，将多条发文拆解为 Rails 期待的单条参数，循环推送
				if res.Status == "normal" && len(res.Posts) > 0 {
					if updateEndpoint == "" {
						logger.Print("POSTS_UPD", fmt.Sprintf("未配置发文更新接口，跳过同步 (account_id=%d)", acc.ID))
					} else {
						successCount := 0
						for _, post := range res.Posts {
							// 将各平台提取的日期归一化为 Rails 能解析的标准字符串
							postDate := normalizePostDate(post.PublishTime)

							// 转换为 Rails 对应的单条结构
							payload := RailsPostParam{
								AccountID:     int64(acc.ID),
								PostDate:      postDate,
								URL:           post.Link,
								Title:         post.Title,
								LikesCount:    post.Likes,
								SharesCount:   post.Shares,
								CommentsCount: post.Comments,
								ViewsCount:    post.Views,
								DataUpdatedAt: time.Now().Format(time.RFC3339),
							}

							// 发起请求
							updateErr := callSinglePostUpdateAPI(bgCtx, logger, updateEndpoint, payload)
							if updateErr != nil {
								logger.Print("E", fmt.Sprintf("同步单条推文失败 (URL: %s): %s", post.Link, updateErr.Error()))
								res.UpdateError = updateErr.Error()
							} else {
								successCount++
							}
						}

						if successCount == len(res.Posts) {
							res.UpdateSent = true
						}
						logger.Print("POSTS_UPD", fmt.Sprintf("账号(account_id=%d) 数据推送完毕。成功: %d/%d", acc.ID, successCount, len(res.Posts)))
					}
				}

				results = append(results, res)

				if i < len(req.ActiveAccounts)-1 {
					time.Sleep(2 * time.Second)
				}
			}

			// 3. 收尾清理
			logger.Print("FP4", "任务结束，正在释放标签页与关闭Profile进程...")
			for _, tab := range openedTabs {
				tab.cancel()
			}

			stopProfileWithCleanup(bgCtx, logger, browserCtx, startRes.Host, startRes.Port, startRes.ProfileID, startRes.Info.WebsocketLink)

			normalCount := 0
			for _, res := range results {
				if res.Status == "normal" {
					normalCount++
				}
			}
			return "success", fmt.Sprintf("采集完成 %d/%d 个账号", normalCount, len(results)), results, startRes.ProfileID
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guardExtra(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("fetch", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, results, _ := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "fetch",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
					Result:      results,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info, results, profileID := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, FetchPostsResponse{
			Type:      "success",
			ProfileID: profileID,
			Results:   results,
		})
	}
}

// normalizePostDate 将各平台抓取到的发文时间归一化为 Rails 可解析的 RFC3339 字符串。
// 各平台返回的日期格式不一：Twitter/Instagram 是 ISO8601("2026-09-15T05:30:00.000Z")，
// TikTok/YouTube 是 "YYYY-MM-DD HH:mm:ss"，Facebook 是绝对日期文本；提取失败时可能是
// 空串或 "Unknown"。统一解析后输出 RFC3339，无法解析的降级为当前时间。
func normalizePostDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "Unknown") || strings.EqualFold(s, "无标题") {
		return time.Now().Format(time.RFC3339)
	}
	layouts := []string{
		time.RFC3339,                    // "2006-01-02T15:04:05Z07:00"（可含小数秒）
		"2006-01-02T15:04:05.000Z07:00", // ISO 带毫秒+时区
		"2006-01-02 15:04:05",           // "YYYY-MM-DD HH:mm:ss"
		"2006-01-02",                    // 纯日期
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	// 无法解析的格式，降级为当前时间
	return time.Now().Format(time.RFC3339)
}

func callSinglePostUpdateAPI(ctx context.Context, logger *logx.Logger, endpoint string, payload RailsPostParam) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", accountCheckUA)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Rails返回错误码 %d: %s", resp.StatusCode, string(raw))
	}

	return nil
}

// handleSendMessage POST /accounts/send_message 已移除(批量发送不再需要, 只保留单条发送)

// handleSendSingleMessage POST /accounts/send_single_message 主动给单个对方账号发私信(非批量)
func handleSendSingleMessage(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req SendSingleMessageRequest
		_, err := decodeJSONBody(r, &req, 1<<20)
		if err != nil {
			logger.Print("MSG", "send_single_message JSON解析失败: "+err.Error())
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}

		if req.ProfileName == "" || req.Platform == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name and platform are required"})
			return
		}
		if req.TargetURL == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "target_url is required"})
			return
		}
		if req.MessageContent == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "message_content is required"})
			return
		}
		if req.AccountID <= 0 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "account_id is required"})
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

		target := req.TargetURL
		if target == "" {
			target = req.AccountName
		}
		logger.Print("MSG", fmt.Sprintf("收到单条私信请求: profile=%s, platform=%s, account_id=%d, target=%s, async=%v", req.ProfileName, req.Platform, req.AccountID, target, req.Async))

		// execute 执行完整私信流程：并发额度 → profile锁 → 启动(带重试) → 发送 → 关闭。
		// 返回 (status, info, resp)：status 为 success/failed；resp 为同步响应结构。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, SendSingleMessageResponse) {
			bgCtx := taskCtx

			// 构造只含一个任务的切片
			tasks := []message.SendTask{{
				TargetURL:      req.TargetURL,
				AccountName:    req.AccountName,
				MessageContent: req.MessageContent,
				Passcode:       req.Passcode,
			}}

			// 获取全局并发额度（排队等待）
			if err := acquireBrowserSlot(bgCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), SendSingleMessageResponse{Type: "error", AccountID: req.AccountID, ErrorInfo: "获取并发额度失败: " + err.Error()}
			}
			defer releaseBrowserSlot()

			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			startRes, err := startProfileByNameWithRetry(bgCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("MSG", "启动Profile失败: "+err.Error())
				return "failed", err.Error(), SendSingleMessageResponse{Type: "error", AccountID: req.AccountID, ErrorInfo: err.Error()}
			}

			allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(bgCtx, startRes.Info.WebsocketLink, chromedp.NoModifyURL)
			defer cancelAlloc()
			browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
				chromedp.WithLogf(func(string, ...interface{}) {}),
				chromedp.WithErrorf(func(string, ...interface{}) {}),
			)
			defer cancelBrowser()
			chromedputil.CleanExtraTabs(browserCtx, logger, "MSG")

			msgCtx, cancelMsg := context.WithTimeout(browserCtx, 5*time.Minute)
			defer cancelMsg()
			sendRes, sendErr := sendMessageByPlatform(msgCtx, logger, req.Platform, tasks)

			for i := range sendRes.Results {
				sendRes.Results[i].ErrorInfo = scraper.SanitizeString(sendRes.Results[i].ErrorInfo)
				sendRes.Results[i].TargetURL = scraper.SanitizeString(sendRes.Results[i].TargetURL)
			}
			sendRes.ErrorInfo = scraper.SanitizeString(sendRes.ErrorInfo)

			stopProfileWithCleanup(bgCtx, logger, browserCtx, startRes.Host, startRes.Port, startRes.ProfileID, startRes.Info.WebsocketLink)

			var outcome *message.SendOutcome
			if len(sendRes.Results) > 0 {
				o := sendRes.Results[0]
				outcome = &o
			}

			if sendErr != nil {
				logger.Print("MSG", "单条私信流程失败: "+sendErr.Error())
				return "failed", sendRes.ErrorInfo + " | " + sanitizeErr(sendErr), SendSingleMessageResponse{
					Type:      "error",
					ProfileID: startRes.ProfileID,
					AccountID: req.AccountID,
					Status:    sendRes.Status,
					Result:    outcome,
					ErrorInfo: sendRes.ErrorInfo + " | " + sanitizeErr(sendErr),
				}
			}

			logger.Print("MSG", fmt.Sprintf("单条私信流程完成: profile=%s, platform=%s, account_id=%d, 状态=%s", req.ProfileName, req.Platform, req.AccountID, sendRes.Status))
			return "success", sendRes.Status, SendSingleMessageResponse{
				Type:      "success",
				ProfileID: startRes.ProfileID,
				AccountID: req.AccountID,
				Status:    sendRes.Status,
				Result:    outcome,
				ErrorInfo: sendRes.ErrorInfo,
			}
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("send_message", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, resp := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "send_message",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
					Result:      resp,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		_, _, resp := execute()
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleFetchMessages POST /accounts/fetch_messages 已移除(不再需要拉取会话列表)

// handleCheckReply POST /accounts/check_reply 判断对方是否回复(单个对方账号)
func handleCheckReply(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req CheckReplyRequest
		_, err := decodeJSONBody(r, &req, 1<<20)
		if err != nil {
			logger.Print("MSG", "check_reply JSON解析失败: "+err.Error())
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}

		if req.ProfileName == "" || req.Platform == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name and platform are required"})
			return
		}
		if req.TargetURL == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "target_url is required"})
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

		target := req.TargetURL
		logger.Print("MSG", fmt.Sprintf("收到判断回复请求: profile=%s, platform=%s, account_id=%d, target=%s, since=%d, async=%v",
			req.ProfileName, req.Platform, req.AccountID, target, req.SinceIncomingCount, req.Async))

		// execute 执行完整判断回复流程：并发额度 → profile锁 → 启动(带重试) → 检查 → 关闭。
		// 返回 (status, info, resp)：status 为 success/failed；resp 为同步响应结构。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, CheckReplyResponse) {
			bgCtx := taskCtx

			if err := acquireBrowserSlot(bgCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), CheckReplyResponse{Type: "error", AccountID: req.AccountID, ErrorInfo: "获取并发额度失败: " + err.Error()}
			}
			defer releaseBrowserSlot()

			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			startRes, err := startProfileByNameWithRetry(bgCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("MSG", "启动Profile失败: "+err.Error())
				return "failed", err.Error(), CheckReplyResponse{Type: "error", AccountID: req.AccountID, ErrorInfo: err.Error()}
			}

			allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(bgCtx, startRes.Info.WebsocketLink, chromedp.NoModifyURL)
			defer cancelAlloc()
			browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
				chromedp.WithLogf(func(string, ...interface{}) {}),
				chromedp.WithErrorf(func(string, ...interface{}) {}),
			)
			defer cancelBrowser()
			chromedputil.CleanExtraTabs(browserCtx, logger, "MSG")

			checkCtx, cancelCheck := context.WithTimeout(browserCtx, 5*time.Minute)
			defer cancelCheck()
			checkRes, checkErr := checkReplyByPlatform(checkCtx, logger, req.Platform, message.CheckReplyOptions{
				TargetURL:          req.TargetURL,
				AccountName:        req.AccountName,
				Passcode:           req.Passcode,
				SinceIncomingCount: req.SinceIncomingCount,
			})

			stopProfileWithCleanup(bgCtx, logger, browserCtx, startRes.Host, startRes.Port, startRes.ProfileID, startRes.Info.WebsocketLink)

			for i := range checkRes.Replies {
				checkRes.Replies[i].SenderName = scraper.SanitizeString(checkRes.Replies[i].SenderName)
				checkRes.Replies[i].Content = scraper.SanitizeString(checkRes.Replies[i].Content)
				checkRes.Replies[i].SentAt = scraper.SanitizeString(checkRes.Replies[i].SentAt)
			}
			checkRes.ErrorInfo = scraper.SanitizeString(checkRes.ErrorInfo)

			if checkErr != nil {
				logger.Print("MSG", "判断回复流程失败: "+checkErr.Error())
				return "failed", checkRes.ErrorInfo + " | " + sanitizeErr(checkErr), CheckReplyResponse{
					Type:        "error",
					ProfileID:   startRes.ProfileID,
					AccountID:   req.AccountID,
					Status:      checkRes.Status,
					ReplyStatus: checkRes.ReplyStatus,
					HasReply:    checkRes.HasReply,
					ReplyCount:  checkRes.ReplyCount,
					Replies:     checkRes.Replies,
					CheckedAt:   checkRes.CheckedAt,
					ErrorInfo:   checkRes.ErrorInfo + " | " + sanitizeErr(checkErr),
				}
			}

			logger.Print("MSG", fmt.Sprintf("判断回复完成: profile=%s, platform=%s, account_id=%d, 状态=%s, 回复状态=%s, 新回复=%d",
				req.ProfileName, req.Platform, req.AccountID, checkRes.Status, checkRes.ReplyStatus, checkRes.ReplyCount))
			return "success", checkRes.ReplyStatus, CheckReplyResponse{
				Type:        "success",
				ProfileID:   startRes.ProfileID,
				AccountID:   req.AccountID,
				Status:      checkRes.Status,
				ReplyStatus: checkRes.ReplyStatus,
				HasReply:    checkRes.HasReply,
				ReplyCount:  checkRes.ReplyCount,
				Replies:     checkRes.Replies,
				CheckedAt:   checkRes.CheckedAt,
				ErrorInfo:   checkRes.ErrorInfo,
			}
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("check_reply", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, resp := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "check_reply",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
					Result:      resp,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		_, _, resp := execute()
		writeJSON(w, http.StatusOK, resp)
	}
}

func main() {
	logger := logx.New(os.Stdout)
	defer logger.Close()

	// 先加载上次运行留下的任务记录（未跑完的任务会被标记为 interrupted），
	// 这样重启后 /tasks 仍能查到历史进度，而不是一片空白。
	loadTaskStore(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 任务状态查询（按 task_id 查询，作为回调失败/重启丢任务时的补充排查手段）
	// 任务查询与清理：
	//   GET  /tasks          任务总览与进度列表
	//   GET  /tasks/summary  任务统计（纯聚合，供 account_sys 看板使用，无明细）
	//   GET  /tasks/{id}     单个任务详情
	//   POST /tasks/clear    中断并清空所有任务
	mux.HandleFunc("/tasks", handleTaskList(logger))
	mux.HandleFunc("/tasks/summary", handleTaskSummary(logger))
	mux.HandleFunc("/tasks/clear", handleTaskClear(logger))
	mux.HandleFunc("/tasks/", handleTaskQuery(logger))

	mux.HandleFunc("/accounts/check_login_status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req AccountCheckRequest
		if r.ContentLength > 0 {
			raw, err := decodeJSONBody(r, &req, 1<<20)
			if err != nil {
				logger.Print("E", "JSON解析失败: "+err.Error())
				logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
				if raw != "" {
					logger.Print("E", "Body: "+safeSnippet(raw, 600))
				}
				writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
				return
			}
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

		items, err := fetchAccountList(r.Context())
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}

		results := make([]AccountCheckResult, 0, len(items))
		for i, item := range items {
			result := checkAccountLogin(r.Context(), logger, item, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			statusDesp := result.StatusDesp
			if result.Status == "normal" {
				statusDesp = ""
			}
			if err := updateAccountStatus(r.Context(), item.ID, statusDesp); err != nil {
				logger.Print("E", "更新状态失败: "+err.Error())
			}
			results = append(results, result)
			if i < len(items)-1 {
				time.Sleep(5 * time.Second)
			}
		}

		writeJSON(w, http.StatusOK, AccountCheckResponse{
			Type:    "success",
			Results: results,
		})
	})

	mux.HandleFunc("/accounts/fetch_posts", handleFetchPosts(logger))

	type NurtureRequest struct {
		ProfileName      string `json:"profile_name"`
		Platform         string `json:"platform"`
		Host             string `json:"host"`
		Port             int    `json:"port"`
		WaitSeconds      int    `json:"wait_seconds"`
		UndetectablePath string `json:"undetectable_path"`
		Async            bool   `json:"async"` // true 时立即返回 task_id、后台执行、完成后回调
		Ref              string `json:"ref,omitempty"`
		Batch            string `json:"batch,omitempty"`
	}

	type NurtureResponse struct {
		Status string `json:"status"`
		Info   string `json:"info"`
	}

	mux.HandleFunc("/accounts/nurture", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req NurtureRequest
		_, err := decodeJSONBody(r, &req, 1<<20)
		if err != nil {
			logger.Print("E", "nurture JSON解析失败: "+err.Error())
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}

		if req.ProfileName == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if req.Platform == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "platform is required"})
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

		logger.Print("NURTURE", fmt.Sprintf("收到养号请求: profile=%s, platform=%s, async=%v", req.ProfileName, req.Platform, req.Async))

		// execute 执行完整养号流程：并发额度 → profile锁 → 启动(带重试) → 养号 → 关闭+释放。
		// 返回 (status, info)，status 为 success / error / failed。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string) {
			bgCtx := taskCtx

			// 1. 获取全局并发额度（超出的任务在此排队等待）
			if err := acquireBrowserSlot(bgCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error()
			}
			defer releaseBrowserSlot()

			// 2. 获取 Profile 操作锁（同一浏览器串行，防止并发操作导致混乱）
			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			// 3. 启动浏览器（带重试）
			res, err := startProfileByNameWithRetry(bgCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", "启动Profile失败: "+err.Error())
				return "failed", err.Error()
			}

			// 4. 建立 CDP 连接
			allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(bgCtx, res.Info.WebsocketLink, chromedp.NoModifyURL)
			defer cancelAlloc()
			browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
				chromedp.WithLogf(func(string, ...interface{}) {}),
				chromedp.WithErrorf(func(string, ...interface{}) {}),
			)
			defer cancelBrowser()
			chromedputil.CleanExtraTabs(browserCtx, logger, "NURTURE")

			// 5. 养号
			nurtureCtx, cancelNurture := context.WithTimeout(browserCtx, 30*time.Minute)
			defer cancelNurture()
			nurtureRes, nurtureErr := nurtureByPlatform(nurtureCtx, logger, req.Platform, nurture.NurtureRequest{
				ProfileName:      req.ProfileName,
				Platform:         req.Platform,
				Host:             req.Host,
				Port:             req.Port,
				WaitSeconds:      req.WaitSeconds,
				UndetectablePath: req.UndetectablePath,
			})

			// 6. 关闭浏览器 + 释放占用
			stopProfileWithCleanup(bgCtx, logger, browserCtx, res.Host, res.Port, res.ProfileID, res.Info.WebsocketLink)

			if nurtureErr != nil {
				logger.Print("E", "养号流程失败: "+nurtureErr.Error())
				return "error", nurtureRes.ErrorInfo + " | " + nurtureRes.ActionsPerformed
			}
			logger.Print("NURTURE", fmt.Sprintf("养号流程完成: profile=%s, platform=%s", req.ProfileName, req.Platform))
			if nurtureRes.Status == "error" {
				return "error", nurtureRes.ErrorInfo + " | " + nurtureRes.ActionsPerformed
			}
			return "success", nurtureRes.ActionsPerformed
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guardVoid(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("nurture", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "nurture",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info := execute()
		writeJSON(w, http.StatusOK, NurtureResponse{Status: status, Info: info})
	})

	mux.HandleFunc("/undetectable/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req StartProfileRequest
		raw, err := decodeJSONBody(r, &req, 1<<20)
		if err != nil {
			logger.Print("E", "JSON解析失败: "+err.Error())
			logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
			if raw != "" {
				logger.Print("E", "Body: "+safeSnippet(raw, 600))
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}

		if req.ProfileName == "" {
			req.ProfileName = "banyun_fb_001"
		}

		res, err := startProfileByName(r.Context(), logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}

		logger.Print("6", "完成")
		writeJSON(w, http.StatusOK, StartProfileResponse{
			Type:             "success",
			ProfileID:        res.ProfileID,
			Status:           res.Info.Status,
			DebugPort:        res.Info.DebugPort,
			WebsocketLink:    res.Info.WebsocketLink,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
		})
	})

	type FacebookPublishRequest struct {
		ProfileName      string `json:"profile_name"`
		Title            string `json:"title"`
		VideoOssURL      string `json:"video_oss_url"`
		VideoPath        string `json:"video_path"`
		Host             string `json:"host"`
		Port             int    `json:"port"`
		WaitSeconds      int    `json:"wait_seconds"`
		UndetectablePath string `json:"undetectable_path"`
		Async            bool   `json:"async"`
		Ref              string `json:"ref,omitempty"`
		Batch            string `json:"batch,omitempty"`
	}

	type FacebookPublishResponse struct {
		Type             string `json:"type"`
		ProfileID        string `json:"profile_id"`
		DebugPort        string `json:"debug_port"`
		WebsocketLink    string `json:"websocket_link"`
		Status           string `json:"status"`
		UndetectableHost string `json:"undetectable_host"`
		UndetectablePort int    `json:"undetectable_port"`
		ErrorInfo        string `json:"error_info,omitempty"`
	}

	mux.HandleFunc("/facebook/publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		var req FacebookPublishRequest
		raw, err := decodeJSONBody(r, &req, 2<<20)
		if err != nil {
			logger.Print("E", "JSON解析失败: "+err.Error())
			logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
			if raw != "" {
				logger.Print("E", "Body: "+safeSnippet(raw, 1200))
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}
		logger.Print("FB_REQ", "Content-Type: "+r.Header.Get("Content-Type"))
		logger.Print("FB_REQ", "Body: "+safeSnippet(raw, 1200))
		if req.ProfileName == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if req.VideoOssURL == "" && req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_oss_url or video_path is required"})
			return
		}

		// execute 执行完整发布流程：并发额度 → profile锁 → 下载视频 → 启动(带重试) → 发布 → 关闭释放。
		// 返回 (status, info, res)：status 为 success/failed；res 为成功启动的浏览器信息（失败时零值）。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, startByNameResult) {
			// 1. 获取全局并发额度（排队用无 deadline 的 taskCtx：排队等待不计入执行超时，
			//    否则队列积压时后面的任务还没轮到就会报 context deadline exceeded）
			if err := acquireBrowserSlot(taskCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
			}
			defer releaseBrowserSlot()

			// 2. 拿到槽位后才起执行超时：publishTimeout 只覆盖下载/启动/发布/关闭，不含排队
			pubCtx, cancelPub := context.WithTimeout(taskCtx, publishTimeout)
			defer cancelPub()

			// 2. 获取 Profile 操作锁
			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			// 3. 下载视频
			var absVideoPath string
			if req.VideoOssURL != "" {
				var err error
				absVideoPath, err = downloadVideoFromOss(pubCtx, logger, req.VideoOssURL)
				if err != nil {
					return "failed", err.Error(), startByNameResult{}
				}
			} else {
				var err error
				absVideoPath, err = filepath.Abs(req.VideoPath)
				if err != nil {
					return "failed", "invalid video_path: " + err.Error(), startByNameResult{}
				}
				if _, err := os.Stat(absVideoPath); err != nil {
					return "failed", "video_path file not found", startByNameResult{}
				}
			}
			defer os.Remove(absVideoPath)

			// 4. 启动浏览器（带重试）
			res, err := startProfileByNameWithRetry(pubCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", err.Error())
				return "failed", err.Error(), startByNameResult{}
			}

			var stopOnce sync.Once
			stopProfile := func(reason string) {
				stopOnce.Do(func() {
					logger.Print("FB", "停止Profile: "+reason)
					stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
					_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
					cancelStop()
				})
			}
			logger.Print("FB", "开始Facebook发布流程")
			pubErr := facebook.PublishVideo(pubCtx, logger, facebook.PublishRequest{
				WebsocketURL:     res.Info.WebsocketLink,
				Title:            req.Title,
				VideoPath:        absVideoPath,
				UndetectableHost: res.Host,
				UndetectablePort: res.Port,
				ProfileID:        res.ProfileID,
			})
			if pubErr != nil {
				logger.Print("E", pubErr.Error())
				stopProfile("publish error")
				return "failed", pubErr.Error(), startByNameResult{}
			}
			stopProfile("publish success")
			return "success", "publish_triggered", res
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("facebook_publish", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, _ := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "facebook_publish",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info, res := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, FacebookPublishResponse{
			Type:             "success",
			ProfileID:        res.ProfileID,
			DebugPort:        res.Info.DebugPort,
			WebsocketLink:    res.Info.WebsocketLink,
			Status:           "publish_triggered",
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
		})
	})

	mux.HandleFunc("/twitter/publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		type TwitterPublishRequest struct {
			ProfileName      string `json:"profile_name"`
			Text             string `json:"text"`
			Title            string `json:"title"`
			VideoOssURL      string `json:"video_oss_url"`
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
			Async            bool   `json:"async"`
			Ref              string `json:"ref,omitempty"`
			Batch            string `json:"batch,omitempty"`
		}
		type TwitterPublishResponse struct {
			Type             string `json:"type"`
			ProfileID        string `json:"profile_id"`
			DebugPort        string `json:"debug_port"`
			WebsocketLink    string `json:"websocket_link"`
			Status           string `json:"status"`
			UndetectableHost string `json:"undetectable_host"`
			UndetectablePort int    `json:"undetectable_port"`
			ErrorInfo        string `json:"error_info,omitempty"`
		}
		var req TwitterPublishRequest
		raw, err := decodeJSONBody(r, &req, 2<<20)
		if err != nil {
			logger.Print("E", "JSON解析失败: "+err.Error())
			logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
			if raw != "" {
				logger.Print("E", "Body: "+safeSnippet(raw, 1200))
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}
		logger.Print("TW_REQ", "Content-Type: "+r.Header.Get("Content-Type"))
		logger.Print("TW_REQ", "Body: "+safeSnippet(raw, 1200))
		if req.ProfileName == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if req.VideoOssURL == "" && req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_oss_url or video_path is required"})
			return
		}

		// execute 执行完整发布流程：并发额度 → profile锁 → 下载视频 → 启动(带重试) → 发布 → 关闭释放。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, startByNameResult) {
			// 1. 获取全局并发额度（排队用无 deadline 的 taskCtx：排队等待不计入执行超时，
			//    否则队列积压时后面的任务还没轮到就会报 context deadline exceeded）
			if err := acquireBrowserSlot(taskCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
			}
			defer releaseBrowserSlot()

			// 2. 拿到槽位后才起执行超时：publishTimeout 只覆盖下载/启动/发布/关闭，不含排队
			pubCtx, cancelPub := context.WithTimeout(taskCtx, publishTimeout)
			defer cancelPub()

			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			var absVideoPath string
			if req.VideoOssURL != "" {
				var err error
				absVideoPath, err = downloadVideoFromOss(pubCtx, logger, req.VideoOssURL)
				if err != nil {
					return "failed", err.Error(), startByNameResult{}
				}
			} else {
				var err error
				absVideoPath, err = filepath.Abs(req.VideoPath)
				if err != nil {
					return "failed", "invalid video_path: " + err.Error(), startByNameResult{}
				}
				if _, err := os.Stat(absVideoPath); err != nil {
					return "failed", "video_path file not found", startByNameResult{}
				}
			}
			defer os.Remove(absVideoPath)

			res, err := startProfileByNameWithRetry(pubCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", err.Error())
				return "failed", err.Error(), startByNameResult{}
			}
			logger.Print("TW", "开始Twitter发布流程")
			textToUse := req.Text
			if strings.TrimSpace(textToUse) == "" && strings.TrimSpace(req.Title) != "" {
				textToUse = req.Title
			}
			pubErr := twitter.PublishVideo(pubCtx, logger, twitter.PublishRequest{
				WebsocketURL:     res.Info.WebsocketLink,
				Text:             textToUse,
				VideoPath:        absVideoPath,
				UndetectableHost: res.Host,
				UndetectablePort: res.Port,
				ProfileID:        res.ProfileID,
			})
			if pubErr != nil {
				logger.Print("E", pubErr.Error())
				stopCtx, cancelStop := context.WithTimeout(pubCtx, 6*time.Second)
				_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
				cancelStop()
				return "failed", pubErr.Error(), startByNameResult{}
			}
			stopCtx, cancelStop := context.WithTimeout(pubCtx, 6*time.Second)
			_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
			cancelStop()
			return "success", "publish_triggered", res
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("twitter_publish", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, _ := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "twitter_publish",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info, res := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, TwitterPublishResponse{
			Type:             "success",
			ProfileID:        res.ProfileID,
			DebugPort:        res.Info.DebugPort,
			WebsocketLink:    res.Info.WebsocketLink,
			Status:           "publish_triggered",
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
		})
	})
	mux.HandleFunc("/youtube/publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		type YouTubePublishRequest struct {
			ProfileName      string `json:"profile_name"`
			Text             string `json:"text"`
			Title            string `json:"title"`
			Description      string `json:"description"`
			VideoOssURL      string `json:"video_oss_url"`
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
			Async            bool   `json:"async"`
			Ref              string `json:"ref,omitempty"`
			Batch            string `json:"batch,omitempty"`
		}
		type YouTubePublishResponse struct {
			Type             string `json:"type"`
			ProfileID        string `json:"profile_id"`
			DebugPort        string `json:"debug_port"`
			WebsocketLink    string `json:"websocket_link"`
			Status           string `json:"status"`
			UndetectableHost string `json:"undetectable_host"`
			UndetectablePort int    `json:"undetectable_port"`
			ErrorInfo        string `json:"error_info,omitempty"`
		}
		var req YouTubePublishRequest
		raw, err := decodeJSONBody(r, &req, 2<<20)
		if err != nil {
			logger.Print("E", "JSON解析失败: "+err.Error())
			logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
			if raw != "" {
				logger.Print("E", "Body: "+safeSnippet(raw, 1200))
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}
		logger.Print("YT_REQ", "Content-Type: "+r.Header.Get("Content-Type"))
		logger.Print("YT_REQ", "Body: "+safeSnippet(raw, 1200))
		if req.ProfileName == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if req.VideoOssURL == "" && req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_oss_url or video_path is required"})
			return
		}

		// execute 执行完整发布流程：并发额度 → profile锁 → 下载视频 → 启动(带重试) → 发布 → 关闭释放。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, startByNameResult) {
			// 1. 获取全局并发额度（排队用无 deadline 的 taskCtx：排队等待不计入执行超时，
			//    否则队列积压时后面的任务还没轮到就会报 context deadline exceeded）
			if err := acquireBrowserSlot(taskCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
			}
			defer releaseBrowserSlot()

			// 2. 拿到槽位后才起执行超时：publishTimeout 只覆盖下载/启动/发布/关闭，不含排队
			pubCtx, cancelPub := context.WithTimeout(taskCtx, publishTimeout)
			defer cancelPub()

			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			var absVideoPath string
			if req.VideoOssURL != "" {
				var err error
				absVideoPath, err = downloadVideoFromOss(pubCtx, logger, req.VideoOssURL)
				if err != nil {
					return "failed", err.Error(), startByNameResult{}
				}
			} else {
				var err error
				absVideoPath, err = filepath.Abs(req.VideoPath)
				if err != nil {
					return "failed", "invalid video_path: " + err.Error(), startByNameResult{}
				}
				if _, err := os.Stat(absVideoPath); err != nil {
					return "failed", "video_path file not found", startByNameResult{}
				}
			}
			defer os.Remove(absVideoPath)

			res, err := startProfileByNameWithRetry(pubCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", err.Error())
				return "failed", err.Error(), startByNameResult{}
			}
			logger.Print("YT", "开始YouTube发布流程")
			titleToUse := strings.TrimSpace(req.Title)
			if titleToUse == "" && strings.TrimSpace(req.Text) != "" {
				titleToUse = req.Text
			}
			pubErr := youtube.PublishVideo(pubCtx, logger, youtube.PublishRequest{
				WebsocketURL:     res.Info.WebsocketLink,
				Title:            titleToUse,
				Description:      req.Description,
				VideoPath:        absVideoPath,
				UndetectableHost: res.Host,
				UndetectablePort: res.Port,
				ProfileID:        res.ProfileID,
			})
			if pubErr != nil {
				logger.Print("E", pubErr.Error())
				return "failed", pubErr.Error(), startByNameResult{}
			}
			time.Sleep(8 * time.Second)
			stopCtx, cancelStop := context.WithTimeout(pubCtx, 6*time.Second)
			_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
			cancelStop()
			return "success", "publish_triggered", res
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("youtube_publish", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, _ := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "youtube_publish",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info, res := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, YouTubePublishResponse{
			Type:             "success",
			ProfileID:        res.ProfileID,
			DebugPort:        res.Info.DebugPort,
			WebsocketLink:    res.Info.WebsocketLink,
			Status:           "publish_triggered",
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
		})
	})
	mux.HandleFunc("/tiktok/publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		type TikTokPublishRequest struct {
			ProfileName      string `json:"profile_name"`
			Text             string `json:"text"`
			Title            string `json:"title"`
			VideoOssURL      string `json:"video_oss_url"`
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
			Async            bool   `json:"async"`
			Ref              string `json:"ref,omitempty"`
			Batch            string `json:"batch,omitempty"`
		}
		type TikTokPublishResponse struct {
			Type             string `json:"type"`
			ProfileID        string `json:"profile_id"`
			DebugPort        string `json:"debug_port"`
			WebsocketLink    string `json:"websocket_link"`
			Status           string `json:"status"`
			UndetectableHost string `json:"undetectable_host"`
			UndetectablePort int    `json:"undetectable_port"`
			ErrorInfo        string `json:"error_info,omitempty"`
		}
		var req TikTokPublishRequest
		raw, err := decodeJSONBody(r, &req, 2<<20)
		if err != nil {
			logger.Print("E", "JSON解析失败: "+err.Error())
			logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
			if raw != "" {
				logger.Print("E", "Body: "+safeSnippet(raw, 1200))
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}
		logger.Print("TT_REQ", "Content-Type: "+r.Header.Get("Content-Type"))
		logger.Print("TT_REQ", "Body: "+safeSnippet(raw, 1200))
		if req.ProfileName == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if req.VideoOssURL == "" && req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_oss_url or video_path is required"})
			return
		}

		// execute 执行完整发布流程：并发额度 → profile锁 → 下载视频 → 启动(带重试) → 发布 → 关闭释放。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, startByNameResult) {
			// 1. 获取全局并发额度（排队用无 deadline 的 taskCtx：排队等待不计入执行超时，
			//    否则队列积压时后面的任务还没轮到就会报 context deadline exceeded）
			if err := acquireBrowserSlot(taskCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
			}
			defer releaseBrowserSlot()

			// 2. 拿到槽位后才起执行超时：publishTimeout 只覆盖下载/启动/发布/关闭，不含排队
			pubCtx, cancelPub := context.WithTimeout(taskCtx, publishTimeout)
			defer cancelPub()

			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			var absVideoPath string
			if req.VideoOssURL != "" {
				var err error
				absVideoPath, err = downloadVideoFromOss(pubCtx, logger, req.VideoOssURL)
				if err != nil {
					return "failed", err.Error(), startByNameResult{}
				}
			} else {
				var err error
				absVideoPath, err = filepath.Abs(req.VideoPath)
				if err != nil {
					return "failed", "invalid video_path: " + err.Error(), startByNameResult{}
				}
				if _, err := os.Stat(absVideoPath); err != nil {
					return "failed", "video_path file not found", startByNameResult{}
				}
			}
			defer os.Remove(absVideoPath)

			res, err := startProfileByNameWithRetry(pubCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", err.Error())
				return "failed", err.Error(), startByNameResult{}
			}
			logger.Print("TT", "开始TikTok发布流程")
			textToUse := strings.TrimSpace(req.Text)
			if textToUse == "" && strings.TrimSpace(req.Title) != "" {
				textToUse = req.Title
			}
			pubErr := tiktok.PublishVideo(pubCtx, logger, tiktok.PublishRequest{
				WebsocketURL:     res.Info.WebsocketLink,
				Text:             textToUse,
				VideoPath:        absVideoPath,
				UndetectableHost: res.Host,
				UndetectablePort: res.Port,
				ProfileID:        res.ProfileID,
			})
			if pubErr != nil {
				logger.Print("E", pubErr.Error())
				return "failed", pubErr.Error(), startByNameResult{}
			}
			time.Sleep(8 * time.Second)
			stopCtx, cancelStop := context.WithTimeout(pubCtx, 6*time.Second)
			_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
			cancelStop()
			return "success", "publish_triggered", res
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("tiktok_publish", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, _ := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "tiktok_publish",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info, res := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, TikTokPublishResponse{
			Type:             "success",
			ProfileID:        res.ProfileID,
			DebugPort:        res.Info.DebugPort,
			WebsocketLink:    res.Info.WebsocketLink,
			Status:           "publish_triggered",
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
		})
	})

	mux.HandleFunc("/instagram/publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		type InstagramPublishRequest struct {
			ProfileName      string `json:"profile_name"`
			Text             string `json:"text"`
			Title            string `json:"title"`
			VideoOssURL      string `json:"video_oss_url"`
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
			Async            bool   `json:"async"`
			Ref              string `json:"ref,omitempty"`
			Batch            string `json:"batch,omitempty"`
		}
		type InstagramPublishResponse struct {
			Type             string `json:"type"`
			ProfileID        string `json:"profile_id"`
			DebugPort        string `json:"debug_port"`
			WebsocketLink    string `json:"websocket_link"`
			Status           string `json:"status"`
			UndetectableHost string `json:"undetectable_host"`
			UndetectablePort int    `json:"undetectable_port"`
			ErrorInfo        string `json:"error_info,omitempty"`
		}
		var req InstagramPublishRequest
		raw, err := decodeJSONBody(r, &req, 2<<20)
		if err != nil {
			logger.Print("E", "JSON解析失败: "+err.Error())
			logger.Print("E", "Content-Type: "+r.Header.Get("Content-Type"))
			if raw != "" {
				logger.Print("E", "Body: "+safeSnippet(raw, 1200))
			}
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "invalid json: " + err.Error()})
			return
		}
		logger.Print("IG_REQ", "Content-Type: "+r.Header.Get("Content-Type"))
		logger.Print("IG_REQ", "Body: "+safeSnippet(raw, 1200))
		if req.ProfileName == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "profile_name is required"})
			return
		}
		if req.VideoOssURL == "" && req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_oss_url or video_path is required"})
			return
		}

		// execute 执行完整发布流程：并发额度 → profile锁 → 下载视频 → 启动(带重试) → 发布 → 关闭释放。
		// taskCtx 为该任务的根 context：异步执行时会被替换为任务自身的可取消 context，
		// 使 POST /tasks/clear 能中断它。同步执行时保持 Background。
		var taskCtx context.Context = context.Background()
		execute := func() (string, string, startByNameResult) {
			// 1. 获取全局并发额度（排队用无 deadline 的 taskCtx：排队等待不计入执行超时，
			//    否则队列积压时后面的任务还没轮到就会报 context deadline exceeded）
			if err := acquireBrowserSlot(taskCtx); err != nil {
				return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
			}
			defer releaseBrowserSlot()

			// 2. 拿到槽位后才起执行超时：publishTimeout 只覆盖下载/启动/发布/关闭，不含排队
			pubCtx, cancelPub := context.WithTimeout(taskCtx, publishTimeout)
			defer cancelPub()

			releaseLock := acquireProfileLock(req.ProfileName, logger)
			defer releaseLock()

			var absVideoPath string
			if req.VideoOssURL != "" {
				var err error
				absVideoPath, err = downloadVideoFromOss(pubCtx, logger, req.VideoOssURL)
				if err != nil {
					return "failed", err.Error(), startByNameResult{}
				}
			} else {
				var err error
				absVideoPath, err = filepath.Abs(req.VideoPath)
				if err != nil {
					return "failed", "invalid video_path: " + err.Error(), startByNameResult{}
				}
				if _, err := os.Stat(absVideoPath); err != nil {
					return "failed", "video_path file not found", startByNameResult{}
				}
			}
			defer os.Remove(absVideoPath)

			res, err := startProfileByNameWithRetry(pubCtx, logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
			if err != nil {
				logger.Print("E", err.Error())
				return "failed", err.Error(), startByNameResult{}
			}
			logger.Print("IG", "开始Instagram发布流程")
			textToUse := req.Text
			if strings.TrimSpace(textToUse) == "" && strings.TrimSpace(req.Title) != "" {
				textToUse = req.Title
			}
			pubErr := instagram.PublishVideo(pubCtx, logger, instagram.PublishRequest{
				WebsocketURL:     res.Info.WebsocketLink,
				Text:             textToUse,
				VideoPath:        absVideoPath,
				UndetectableHost: res.Host,
				UndetectablePort: res.Port,
				ProfileID:        res.ProfileID,
			})
			if pubErr != nil {
				logger.Print("E", pubErr.Error())
				stopCtx, cancelStop := context.WithTimeout(pubCtx, 6*time.Second)
				_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
				cancelStop()
				return "failed", pubErr.Error(), startByNameResult{}
			}
			stopCtx, cancelStop := context.WithTimeout(pubCtx, 6*time.Second)
			_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
			cancelStop()
			return "success", "publish_triggered", res
		}

		// 任务执行统一加 panic 隔离：单个任务内部异常不应终止整个服务进程
		execute = guard(logger, execute)

		// 异步：立即返回 task_id，后台执行，完成后回调
		if req.Async {
			taskID, tctx := registerTask("instagram_publish", req.ProfileName, req.Ref, req.Batch)
			taskCtx = tctx
			writeJSON(w, http.StatusOK, map[string]string{"type": "accepted", "task_id": taskID})
			go func() {
				setTaskState(taskID, taskStatusRunning)
				status, info, _ := execute()
				finalStatus := taskStatusSuccess
				if status != "success" {
					finalStatus = taskStatusFailed
				}
				finishTask(taskID, finalStatus, info)
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      taskID,
					ProfileName: req.ProfileName,
					TaskType:    "instagram_publish",
					Ref:         req.Ref,
					Status:      status,
					Message:     info,
				})
			}()
			return
		}

		// 同步：原地执行并返回
		status, info, res := execute()
		if status != "success" {
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: info})
			return
		}
		writeJSON(w, http.StatusOK, InstagramPublishResponse{
			Type:             "success",
			ProfileID:        res.ProfileID,
			DebugPort:        res.Info.DebugPort,
			WebsocketLink:    res.Info.WebsocketLink,
			Status:           "publish_triggered",
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
		})
	})

	// 私信模块: 主动发消息(单条) + 判断回复 [保留本地精简逻辑，移除批量发送]
	mux.HandleFunc("/accounts/send_single_message", handleSendSingleMessage(logger))
	mux.HandleFunc("/accounts/check_reply", handleCheckReply(logger))

	mux.HandleFunc("/api/browser/locked", chrome.GetLockedProfilesHandler(logger, "127.0.0.1", 25325))

	// 鉴权：API Key 认证 + HMAC-SHA256 请求签名，保护所有发布/消息/账号接口。
	authCfg, err := auth.LoadConfig()
	if err != nil {
		logger.Print("E", err.Error())
		logger.Close() // 同步刷新日志，避免 os.Exit 时异步终端日志丢失
		fmt.Fprintf(os.Stderr, "\n[启动失败] %s\n\n", err.Error())
		fmt.Fprintln(os.Stderr, "请先设置鉴权环境变量再启动：")
		fmt.Fprintln(os.Stderr, "  Windows:  set API_KEY=<你的密钥> && set API_SECRET=<你的签名密钥>")
		fmt.Fprintln(os.Stderr, "  Linux:    export API_KEY=<你的密钥> API_SECRET=<你的签名密钥>")
		fmt.Fprintln(os.Stderr, "  （API_SECRET 可不设，缺省自动复用 API_KEY）")
		os.Exit(1)
	}
	handler := auth.Middleware(authCfg, mux)

	// 默认只监听本机回环地址，由同机 nginx 反代对外提供 HTTPS 访问，
	// 避免公网直连明文 IP:8080。如需调整可用 LISTEN_ADDR 环境变量覆盖。
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	// 任务快照：状态变更后合并写入本地 JSON，并按保留策略定期清理超期记录。
	startTaskStoreWriter(logger)

	// 优雅退出：收到 Ctrl+C / SIGTERM 时先把任务快照刷盘再退出，
	// 避免最后那个合并窗口内的状态变更丢失（被强杀时靠 2 秒合并窗口兜底）。
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		logger.Print("BOOT", "收到退出信号，正在保存任务快照…")
		flushTaskStoreNow(logger)
		logger.Close()
		os.Exit(0)
	}()

	// 启动上报：告知 account_sys「本机已重启」，让它立即重置本机丢失的异步任务，
	// 不必再等 check_timeout_tasks 的 45 分钟兜底窗口。延迟 3 秒等服务真正开始监听。
	// Best Effort：上报失败不影响服务启动，account_sys 侧仍有 45 分钟兜底。
	go func() {
		time.Sleep(3 * time.Second)
		notifyMachineRestarted(logger)
	}()

	logger.Print("BOOT", "listening on "+addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		logger.Print("E", err.Error())
		os.Exit(1)
	}
}
