package undetectable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient(host string, port int) *Client {
	baseURL := fmt.Sprintf("http://%s:%d", host, port)
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

type APIResponse struct {
	Code   int             `json:"code"`
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

type StatusResponse struct{}

type ProfileInfo struct {
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	DebugPort     string   `json:"debug_port"`
	WebsocketLink string   `json:"websocket_link"`
	Folder        string   `json:"folder"`
	Tags          []string `json:"tags"`
	CloudID       string   `json:"cloud_id"`
	CreationDate  int64    `json:"creation_date"`
	ModifyDate    int64    `json:"modify_date"`
}

type Profiles map[string]ProfileInfo

func (c *Client) do(ctx context.Context, method string, path string, query url.Values, body any) (*APIResponse, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, err
	}
	u.Path = path
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResp APIResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w; raw=%s", err, string(raw))
	}

	if resp.StatusCode >= 400 {
		return &apiResp, fmt.Errorf("http %d", resp.StatusCode)
	}

	return &apiResp, nil
}

func (c *Client) Status(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/status", nil, nil)
	if err != nil {
		return err
	}
	if resp.Code != 0 {
		return fmt.Errorf("api status not ok: code=%d status=%s", resp.Code, resp.Status)
	}
	return nil
}

func (c *Client) ListProfiles(ctx context.Context) (Profiles, error) {
	resp, err := c.do(ctx, http.MethodGet, "/list", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("list profiles failed: code=%d status=%s data=%s", resp.Code, resp.Status, string(resp.Data))
	}
	var profiles Profiles
	if err := json.Unmarshal(resp.Data, &profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

func FindProfileIDByName(profiles Profiles, name string) (string, error) {
	var matchID string
	for id, info := range profiles {
		if info.Name == name {
			if matchID != "" && matchID != id {
				return "", fmt.Errorf("multiple profiles with same name=%s", name)
			}
			matchID = id
		}
	}
	if matchID == "" {
		return "", fmt.Errorf("profile not found: name=%s", name)
	}
	return matchID, nil
}

type StartResult struct {
	DebugPort     string `json:"debug_port"`
	WebsocketLink string `json:"websocket_link"`
}

func (c *Client) StartProfileBestEffort(ctx context.Context, profileID string) error {
	candidates := []struct {
		Method string
		Path   string
		Query  url.Values
		Body   any
	}{
		// 常见路径
		{Method: http.MethodGet, Path: "/profile/start", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/start/" + profileID},
		{Method: http.MethodPost, Path: "/profile/start", Body: map[string]string{"profile_id": profileID}},

		// 可能的变体
		{Method: http.MethodGet, Path: "/api/profile/start", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/api/v1/profile/start", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/v1/profile/start", Query: url.Values{"profile_id": []string{profileID}}},

		// launch 动词
		{Method: http.MethodGet, Path: "/profile/launch", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/launch/" + profileID},

		// open 动词
		{Method: http.MethodGet, Path: "/profile/open", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/open/" + profileID},

		// 其他参数名
		{Method: http.MethodGet, Path: "/profile/start", Query: url.Values{"id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/start", Query: url.Values{"profile": []string{profileID}}},
	}

	var errs []string
	for _, cnd := range candidates {
		resp, err := c.do(ctx, cnd.Method, cnd.Path, cnd.Query, cnd.Body)
		if err != nil {
			errs = append(errs, fmt.Sprintf("[%s %s]: %v", cnd.Method, cnd.Path, err))
			continue
		}
		if resp.Code == 0 {
			return nil
		}
		errs = append(errs, fmt.Sprintf("[%s %s]: api code=%d msg=%s", cnd.Method, cnd.Path, resp.Code, string(resp.Data)))
	}

	return fmt.Errorf("all start attempts failed:\n%s", strings.Join(errs, "\n"))
}

// WaitProfileStarted 等待 profile 进入「真正可连接」的状态。
//
// 注意：Undetectable 的 /list 中，status 变为 "Started" 与 websocket_link 的回写是两个阶段
// （先拉起浏览器进程，再探测 CDP 端口并回填链接）。若只判断 status 就返回，会拿到一个
// websocket_link 为空的 ProfileInfo，上层（各平台 PublishVideo、chromedp allocator）随即报
// "websocket_url is required" —— 并发启动时主程序回填更慢，表现为偶发、成批出现。
// 因此这里必须同时满足 status == "Started" 且 websocket_link 非空，才算就绪。
func WaitProfileStarted(ctx context.Context, c *Client, profileID string, timeout time.Duration) (ProfileInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastStatus string
	startedButNoLink := false
	for time.Now().Before(deadline) {
		profiles, err := c.ListProfiles(ctx)
		if err != nil {
			return ProfileInfo{}, err
		}
		if info, ok := profiles[profileID]; ok {
			lastStatus = info.Status
			if info.Status == "Started" {
				if info.WebsocketLink != "" {
					return info, nil
				}
				// 状态已就绪但链接尚未回写：继续轮询等待
				startedButNoLink = true
			}
		}
		select {
		case <-ctx.Done():
			return ProfileInfo{}, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	if startedButNoLink {
		// 状态长期停在 Started 却始终拿不到连接地址：通常是残留实例（主程序重启后链接丢失）
		// 或主程序异常。错误文案固定，便于上层识别并触发重试。
		return ProfileInfo{}, fmt.Errorf("websocket_link is empty after profile started: profile_id=%s timeout=%s", profileID, timeout)
	}
	return ProfileInfo{}, fmt.Errorf("wait profile started timeout: %s (last status=%q)", timeout, lastStatus)
}

func (c *Client) StopProfileBestEffort(ctx context.Context, profileID string) error {
	// 停止 profile 属于清理动作，必须可靠：即使调用方的请求上下文已取消
	// (客户端超时/断开导致 r.Context() 被取消)，也要完成停止，避免浏览器残留。
	// 因此当传入 ctx 已失效时，改用独立的 background context。
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
	}

	candidates := []struct {
		Method string
		Path   string
		Query  url.Values
		Body   any
	}{
		{Method: http.MethodGet, Path: "/profile/stop", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/stop/" + profileID},
		{Method: http.MethodPost, Path: "/profile/stop", Body: map[string]string{"profile_id": profileID}},
		{Method: http.MethodGet, Path: "/profile/close", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/close/" + profileID},
		{Method: http.MethodPost, Path: "/profile/close", Body: map[string]string{"profile_id": profileID}},
		{Method: http.MethodGet, Path: "/api/profile/stop", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/api/v1/profile/stop", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/v1/profile/stop", Query: url.Values{"profile_id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/stop", Query: url.Values{"id": []string{profileID}}},
		{Method: http.MethodGet, Path: "/profile/stop", Query: url.Values{"profile": []string{profileID}}},
	}
	var errs []string
	for _, cnd := range candidates {
		resp, err := c.do(ctx, cnd.Method, cnd.Path, cnd.Query, cnd.Body)
		if err != nil {
			errs = append(errs, fmt.Sprintf("[%s %s]: %v", cnd.Method, cnd.Path, err))
			continue
		}
		if resp.Code == 0 {
			return nil
		}
		errs = append(errs, fmt.Sprintf("[%s %s]: api code=%d msg=%s", cnd.Method, cnd.Path, resp.Code, string(resp.Data)))
	}
	return fmt.Errorf("all stop attempts failed:\n%s", strings.Join(errs, "\n"))
}
func StartLocal(ctx context.Context, exePath string) error {
	if exePath == "" {
		return fmt.Errorf("empty exePath")
	}
	// 不使用 CommandContext，因为 localCtx 会在函数返回后 cancel，导致进程被 kill
	// 我们希望 Undetectable 独立运行
	cmd := exec.Command(exePath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}
