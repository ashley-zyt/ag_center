package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"minimax_pro/internal/logx"
)

// ===== 全局并发控制 =====
// 指纹浏览器非常吃内存/CPU，且可能有真人同时在操作，因此限制同一时刻同时运行的浏览器任务数。
// 超出的任务在信号量上排队等待（不拒绝），由 maxConcurrentBrowsers 控制并发上限。

var (
	maxConcurrentBrowsers = 3
	browserSem            = make(chan struct{}, 3)
)

func init() {
	if v := os.Getenv("MAX_CONCURRENT_BROWSERS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			maxConcurrentBrowsers = n
			browserSem = make(chan struct{}, n)
		}
	}
}

// acquireBrowserSlot 获取一个浏览器执行额度，阻塞排队直到有空位或 ctx 取消。
func acquireBrowserSlot(ctx context.Context) error {
	select {
	case browserSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseBrowserSlot 释放一个浏览器执行额度。
func releaseBrowserSlot() {
	<-browserSem
}

// ===== 任务 ID 与状态 =====

var (
	taskSeq      int64
	taskSeqMu    sync.Mutex
	taskStates   = make(map[string]string)
	taskStatesMu sync.Mutex
)

const (
	taskStatusQueued  = "queued"
	taskStatusRunning = "running"
	taskStatusSuccess = "success"
	taskStatusFailed  = "failed"
)

// newTaskID 生成一个唯一任务 ID。
func newTaskID() string {
	taskSeqMu.Lock()
	taskSeq++
	n := taskSeq
	taskSeqMu.Unlock()
	return fmt.Sprintf("t%d_%d", time.Now().UnixNano(), n)
}

func setTaskState(taskID, status string) {
	taskStatesMu.Lock()
	taskStates[taskID] = status
	taskStatesMu.Unlock()
}

func getTaskState(taskID string) string {
	taskStatesMu.Lock()
	defer taskStatesMu.Unlock()
	return taskStates[taskID]
}

// ===== 启动重试 =====
// 仅针对"指纹浏览器相关"的临时性错误重试；平台操作/页面/下载等错误不重试。

const (
	browserStartRetryTimes    = 3
	browserStartRetryInterval = 10 * time.Second
)

// isRetryableBrowserError 判断错误是否属于"指纹浏览器相关"可重试错误。
func isRetryableBrowserError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "invalid profile id") ||
		strings.Contains(lower, "profile is locked") ||
		strings.Contains(lower, "unable to lock profile") ||
		strings.Contains(lower, "failed to lock profile") ||
		strings.Contains(lower, "cannot lock profile") ||
		strings.Contains(lower, "all start attempts failed") ||
		strings.Contains(lower, "websocket_url is required") ||
		strings.Contains(lower, "无法连接undetectable") ||
		strings.Contains(lower, "已尝试启动undetectable") ||
		strings.Contains(lower, "wait profile started timeout") ||
		strings.Contains(lower, "context deadline exceeded")
}

// ===== 任务结果回调 =====

// taskResultURL 任务完成后的回调地址（已与 account_sys 确认）。
const taskResultURL = "http://47.89.235.227:3366/api/v1/browser_tasks/result"

// TaskResultPayload 任务完成回调的 payload。
type TaskResultPayload struct {
	TaskID      string `json:"task_id"`
	ProfileName string `json:"profile_name"`
	TaskType    string `json:"task_type"`
	Status      string `json:"status"` // success / failed
	Ref         string `json:"ref,omitempty"`
	Message     string `json:"message,omitempty"`
	Result      any    `json:"result,omitempty"`
}

// callbackTaskResult 任务完成后回调 account_sys 通知结果（Best Effort：失败仅打日志）。
func callbackTaskResult(ctx context.Context, logger *logx.Logger, payload TaskResultPayload) {
	if taskResultURL == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Print("TASK_CB", "序列化任务回调失败: "+err.Error())
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, taskResultURL, bytes.NewReader(body))
	if err != nil {
		logger.Print("TASK_CB", "构建任务回调请求失败: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", accountCheckUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Print("TASK_CB", "任务回调失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	logger.Print("TASK_CB", fmt.Sprintf("任务回调完成: status=%d body=%s", resp.StatusCode, string(raw)))
}

// handleTaskQuery GET /tasks/{id} 查询任务状态（作为回调失败/重启丢任务时的补充排查手段）。
func handleTaskQuery(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		taskID := strings.TrimPrefix(r.URL.Path, "/tasks/")
		if taskID == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "task_id is required"})
			return
		}
		status := getTaskState(taskID)
		if status == "" {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Type: "error", ErrorInfo: "task not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID, "status": status})
	}
}
