package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/facebook"
	"minimax_pro/internal/platform/instagram"
	"minimax_pro/internal/platform/tiktok"
	"minimax_pro/internal/platform/twitter"
	"minimax_pro/internal/platform/youtube"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/browser"
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
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
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

	startRes, err := startProfileByName(ctx, logger, item.ProfileName, host, port, waitSeconds, undetectablePath)
	if err != nil {
		res.StatusDesp = err.Error()
		return res
	}

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, startRes.Info.WebsocketLink, chromedp.NoModifyURL)
	defer cancelAlloc()

	tabCtx, cancelTab := chromedp.NewContext(allocCtx)
	defer cancelTab()

	defer func() {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 6*time.Second)
		_ = chromedp.Run(closeCtx, browser.Close())
		cancelClose()

		stopCtx, cancelStop := context.WithTimeout(context.Background(), 6*time.Second)
		_ = undetectable.NewClient(startRes.Host, startRes.Port).StopProfileBestEffort(stopCtx, startRes.ProfileID)
		cancelStop()
	}()

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
		logger.Print("4", "profile已在运行中，跳过启动")
		return startByNameResult{ProfileID: profileID, Info: info, Host: host, Port: port, Path: path}, nil
	}

	logger.Print("4", "启动profile")
	startErr := client.StartProfileBestEffort(localCtx, profileID)
	if startErr == nil {
		logger.Print("4", "启动请求成功")
	} else {
		if strings.Contains(startErr.Error(), "Profile is locked") {
			return startByNameResult{}, fmt.Errorf("指纹浏览器已被占用 (Profile is locked)")
		}
		logger.Print("4", "启动请求异常，尝试继续检测状态")
	}

	logger.Print("5", "等待profile进入 Started 状态")
	info, waitErr := undetectable.WaitProfileStarted(localCtx, client, profileID, time.Duration(waitSeconds)*time.Second)
	if waitErr != nil {
		if startErr != nil {
			return startByNameResult{}, startErr
		}
		return startByNameResult{}, waitErr
	}
	logger.Print("5", "已启动")

	return startByNameResult{ProfileID: profileID, Info: info, Host: host, Port: port, Path: path}, nil
}

func resolveUndetectablePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("UNDETECTABLE_EXE"); v != "" {
		return v
	}
	return ""
}

func ensureAPIAndMaybeStart(ctx context.Context, logger *logx.Logger, host string, port int, waitSeconds int, explicitPath string) (*undetectable.Client, string, error) {
	logger.Print("1", "检查本地API服务")
	client := undetectable.NewClient(host, port)
	localCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds+20)*time.Second)
	defer cancel()
	if err := client.Status(localCtx); err == nil {
		logger.Print("1", "API服务正常")
		return client, "", nil
	}

	path := resolveUndetectablePath(explicitPath)
	if path == "" {
		return nil, "", fmt.Errorf("无法连接Undetectable API且未配置undetectable_path或UNDETECTABLE_EXE")
	}

	logger.Print("BOOT", "尝试启动Undetectable: "+path)
	if err := undetectable.StartLocal(localCtx, path); err != nil {
		return nil, "", fmt.Errorf("启动Undetectable失败: %w", err)
	}

	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for time.Now().Before(deadline) {
		if err := client.Status(localCtx); err == nil {
			logger.Print("1", "Undetectable已启动，API服务正常")
			return client, path, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, path, fmt.Errorf("已尝试启动Undetectable，但在超时时间内API仍不可用")
}

func main() {
	logger := logx.New(os.Stdout)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

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
		VideoPath        string `json:"video_path"`
		Host             string `json:"host"`
		Port             int    `json:"port"`
		WaitSeconds      int    `json:"wait_seconds"`
		UndetectablePath string `json:"undetectable_path"`
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
		if req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_path is required"})
			return
		}
		absVideoPath, err := filepath.Abs(req.VideoPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		if _, err := os.Stat(absVideoPath); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}

		res, err := startProfileByName(r.Context(), logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
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
		go func() {
			select {
			case <-r.Context().Done():
				stopProfile("request canceled")
			}
		}()
		logger.Print("FB", "开始Facebook发布流程")
		if err := facebook.PublishVideo(r.Context(), logger, facebook.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Title:            req.Title,
			VideoPath:        absVideoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		}); err != nil {
			logger.Print("E", err.Error())
			stopProfile("publish error")
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}

		// 成功后也关闭 Profile，释放浏览器
		stopProfile("publish success")

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
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
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
		if req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_path is required"})
			return
		}
		absVideoPath, err := filepath.Abs(req.VideoPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		if _, err := os.Stat(absVideoPath); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		res, err := startProfileByName(r.Context(), logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		logger.Print("TW", "开始Twitter发布流程")
		textToUse := req.Text
		if strings.TrimSpace(textToUse) == "" && strings.TrimSpace(req.Title) != "" {
			textToUse = req.Title
		}
		if err := twitter.PublishVideo(r.Context(), logger, twitter.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Text:             textToUse,
			VideoPath:        absVideoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		}); err != nil {
			logger.Print("E", err.Error())
			stopCtx, cancelStop := context.WithTimeout(r.Context(), 6*time.Second)
			_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
			cancelStop()
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		stopCtx, cancelStop := context.WithTimeout(r.Context(), 6*time.Second)
		_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
		cancelStop()
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
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
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
		if req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_path is required"})
			return
		}
		absVideoPath, err := filepath.Abs(req.VideoPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		if _, err := os.Stat(absVideoPath); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		res, err := startProfileByName(r.Context(), logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		logger.Print("YT", "开始YouTube发布流程")
		titleToUse := strings.TrimSpace(req.Title)
		if titleToUse == "" && strings.TrimSpace(req.Text) != "" {
			titleToUse = req.Text
		}
		if err := youtube.PublishVideo(r.Context(), logger, youtube.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Title:            titleToUse,
			Description:      req.Description,
			VideoPath:        absVideoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		}); err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		time.Sleep(8 * time.Second)
		stopCtx, cancelStop := context.WithTimeout(r.Context(), 6*time.Second)
		_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
		cancelStop()
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
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
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
		if req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_path is required"})
			return
		}
		absVideoPath, err := filepath.Abs(req.VideoPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		if _, err := os.Stat(absVideoPath); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		res, err := startProfileByName(r.Context(), logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		logger.Print("TT", "开始TikTok发布流程")
		textToUse := strings.TrimSpace(req.Text)
		if textToUse == "" && strings.TrimSpace(req.Title) != "" {
			textToUse = req.Title
		}
		if err := tiktok.PublishVideo(r.Context(), logger, tiktok.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Text:             textToUse,
			VideoPath:        absVideoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		}); err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		time.Sleep(8 * time.Second)
		stopCtx, cancelStop := context.WithTimeout(r.Context(), 6*time.Second)
		_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
		cancelStop()
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
			VideoPath        string `json:"video_path"`
			Host             string `json:"host"`
			Port             int    `json:"port"`
			WaitSeconds      int    `json:"wait_seconds"`
			UndetectablePath string `json:"undetectable_path"`
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
		if req.VideoPath == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "video_path is required"})
			return
		}
		absVideoPath, err := filepath.Abs(req.VideoPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		if _, err := os.Stat(absVideoPath); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		res, err := startProfileByName(r.Context(), logger, req.ProfileName, req.Host, req.Port, req.WaitSeconds, req.UndetectablePath)
		if err != nil {
			logger.Print("E", err.Error())
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		logger.Print("IG", "开始Instagram发布流程")
		textToUse := req.Text
		if strings.TrimSpace(textToUse) == "" && strings.TrimSpace(req.Title) != "" {
			textToUse = req.Title
		}
		if err := instagram.PublishVideo(r.Context(), logger, instagram.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Text:             textToUse,
			VideoPath:        absVideoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		}); err != nil {
			logger.Print("E", err.Error())
			stopCtx, cancelStop := context.WithTimeout(r.Context(), 6*time.Second)
			_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
			cancelStop()
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Type: "error", ErrorInfo: err.Error()})
			return
		}
		stopCtx, cancelStop := context.WithTimeout(r.Context(), 6*time.Second)
		_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
		cancelStop()
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

	addr := ":8080"
	logger.Print("BOOT", "listening on "+addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Print("E", err.Error())
		os.Exit(1)
	}
}
