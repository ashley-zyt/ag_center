package undetectable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFindProfileIDByName(t *testing.T) {
	profiles := Profiles{
		"id1": {Name: "a"},
		"id2": {Name: "b"},
	}
	id, err := FindProfileIDByName(profiles, "b")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if id != "id2" {
		t.Fatalf("expected id2, got %s", id)
	}
}

func TestFindProfileIDByName_NotFound(t *testing.T) {
	profiles := Profiles{
		"id1": {Name: "a"},
	}
	_, err := FindProfileIDByName(profiles, "x")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestFindProfileIDByName_Duplicate(t *testing.T) {
	profiles := Profiles{
		"id1": {Name: "dup"},
		"id2": {Name: "dup"},
	}
	_, err := FindProfileIDByName(profiles, "dup")
	if err == nil {
		t.Fatalf("expected error")
	}
}

// ===== 回归测试：start / stop 的端点顺序 =====
//
// 背景（2026-09-28 实测踩坑）：官方 Undetectable Local API 定义的是**路径式**接口
// `GET /profile/stop/{id}` / `GET /profile/start/{id}`。原实现把「查询参数式」
// `/profile/stop?profile_id=...` 排在首位，主程序对此应答
// `{"code":1,"data":{"error":"Invalid profile id"}}` —— 看着像 profile 不存在，
// 实际只是参数形式不对，把排查方向带偏了一整轮，并且每次启动/停止都白吃一次失败请求。
// 这些用例把「官方路径式必须排第一」钉死，防止以后被无意改回去。

func newTestClient(t *testing.T, rawURL string) *Client {
	t.Helper()
	return &Client{BaseURL: rawURL, HTTPClient: http.DefaultClient}
}

// successBody 主程序成功应答（code=0）。
const successBody = `{"code":0,"status":"success","data":{}}`

// invalidIDBody 主程序对「参数形式不对」的应答。
const invalidIDBody = `{"code":1,"status":"error","data":{"error":"Invalid profile id"}}`

func TestStopProfileBestEffort_PrefersPathStyle(t *testing.T) {
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.URL.RequestURI())
		if r.URL.Path == "/profile/stop/p1" {
			_, _ = w.Write([]byte(successBody))
			return
		}
		_, _ = w.Write([]byte(invalidIDBody))
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL).StopProfileBestEffort(context.Background(), "p1"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(reqs) != 1 || reqs[0] != "/profile/stop/p1" {
		t.Fatalf("首选端点必须是官方路径式 /profile/stop/{id}，实际请求: %v", reqs)
	}
}

func TestStopProfileBestEffort_FallsBackWhenPathStyleFails(t *testing.T) {
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.URL.RequestURI())
		if r.URL.Path == "/profile/stop" && r.URL.Query().Get("profile_id") == "p1" {
			_, _ = w.Write([]byte(successBody))
			return
		}
		_, _ = w.Write([]byte(invalidIDBody))
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL).StopProfileBestEffort(context.Background(), "p1"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(reqs) < 2 || reqs[0] != "/profile/stop/p1" || reqs[1] != "/profile/stop?profile_id=p1" {
		t.Fatalf("路径式失败后应回退到查询参数式，实际请求: %v", reqs)
	}
}

func TestStopProfileBestEffort_AllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(invalidIDBody))
	}))
	defer srv.Close()

	err := newTestClient(t, srv.URL).StopProfileBestEffort(context.Background(), "p1")
	if err == nil {
		t.Fatalf("expected error when every candidate fails")
	}
	if !strings.Contains(err.Error(), "all stop attempts failed") {
		t.Fatalf("错误信息应汇总所有尝试，实际: %v", err)
	}
	// 失败信息里不该再出现确认不存在的变体（它们只会刷一屏 404 噪音）
	for _, noise := range []string{"/api/profile/stop", "/api/v1/profile/stop", "/v1/profile/stop"} {
		if strings.Contains(err.Error(), noise) {
			t.Fatalf("失败信息里应已移除无用变体 %s，实际: %v", noise, err)
		}
	}
}

func TestStartProfileBestEffort_PrefersPathStyle(t *testing.T) {
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.URL.RequestURI())
		if r.URL.Path == "/profile/start/p1" {
			_, _ = w.Write([]byte(`{"code":0,"status":"success","data":{"debug_port":"45678"}}`))
			return
		}
		_, _ = w.Write([]byte(invalidIDBody))
	}))
	defer srv.Close()

	if err := newTestClient(t, srv.URL).StartProfileBestEffort(context.Background(), "p1"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(reqs) != 1 || reqs[0] != "/profile/start/p1" {
		t.Fatalf("首选端点必须是官方路径式 /profile/start/{id}，实际请求: %v", reqs)
	}
}

func TestStartProfileBestEffort_AllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(invalidIDBody))
	}))
	defer srv.Close()

	err := newTestClient(t, srv.URL).StartProfileBestEffort(context.Background(), "p1")
	if err == nil {
		t.Fatalf("expected error when every candidate fails")
	}
	if !strings.Contains(err.Error(), "all start attempts failed") {
		t.Fatalf("错误信息应汇总所有尝试，实际: %v", err)
	}
}
