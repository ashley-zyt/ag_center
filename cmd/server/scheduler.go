package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
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
	taskSeq       int64
	taskSeqMu     sync.Mutex
	taskRecords   = make(map[string]*TaskRecord)
	taskCancels   = make(map[string]context.CancelFunc)
	clearedTasks  = make(map[string]bool)
	taskRecordsMu sync.Mutex
)

const (
	taskStatusQueued  = "queued"
	taskStatusRunning = "running"
	taskStatusSuccess = "success"
	taskStatusFailed  = "failed"
)

// TaskRecord 单个任务在内存中的运行记录，用于查询进度。
type TaskRecord struct {
	TaskID      string    `json:"task_id"`
	Type        string    `json:"type"`
	ProfileName string    `json:"profile_name,omitempty"`
	Ref         string    `json:"ref,omitempty"`
	Status      string    `json:"status"`
	Message     string    `json:"message,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// newTaskID 生成一个唯一任务 ID。
func newTaskID() string {
	taskSeqMu.Lock()
	taskSeq++
	n := taskSeq
	taskSeqMu.Unlock()
	return fmt.Sprintf("t%d_%d", time.Now().UnixNano(), n)
}

// registerTask 登记一个新任务（状态 queued），返回 task_id 与该任务的可取消 context。
// 调用方必须把返回的 ctx 作为任务执行的根 context，这样"清空任务队列"时才能中断它。
func registerTask(taskType, profileName, ref string) (string, context.Context) {
	taskID := newTaskID()
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	taskRecordsMu.Lock()
	taskRecords[taskID] = &TaskRecord{
		TaskID:      taskID,
		Type:        taskType,
		ProfileName: profileName,
		Ref:         ref,
		Status:      taskStatusQueued,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	taskCancels[taskID] = cancel
	taskRecordsMu.Unlock()
	return taskID, ctx
}

// setTaskState 更新任务状态（message 保持不变）。
func setTaskState(taskID, status string) {
	taskRecordsMu.Lock()
	if rec, ok := taskRecords[taskID]; ok {
		rec.Status = status
		rec.UpdatedAt = time.Now()
	}
	taskRecordsMu.Unlock()
}

// finishTask 更新任务终态与结果信息，并释放该任务的 cancel 资源。
func finishTask(taskID, status, message string) {
	taskRecordsMu.Lock()
	if rec, ok := taskRecords[taskID]; ok {
		rec.Status = status
		rec.Message = message
		rec.UpdatedAt = time.Now()
	}
	if cancel, ok := taskCancels[taskID]; ok {
		cancel()
		delete(taskCancels, taskID)
	}
	taskRecordsMu.Unlock()
}

func getTaskState(taskID string) string {
	taskRecordsMu.Lock()
	defer taskRecordsMu.Unlock()
	if rec, ok := taskRecords[taskID]; ok {
		return rec.Status
	}
	return ""
}

func getTaskRecord(taskID string) *TaskRecord {
	taskRecordsMu.Lock()
	defer taskRecordsMu.Unlock()
	return taskRecords[taskID]
}

// clearAllTasks 中断所有任务（排队中与正在运行的全部取消），清空任务记录，
// 并把这些任务标记为「已清除」以抑制回调，返回被清理的任务数。
func clearAllTasks() int {
	taskRecordsMu.Lock()
	defer taskRecordsMu.Unlock()
	n := len(taskRecords)
	newCleared := make(map[string]bool, n)
	for id, cancel := range taskCancels {
		cancel()
		newCleared[id] = true
	}
	for id := range taskRecords {
		newCleared[id] = true
	}
	clearedTasks = newCleared
	taskCancels = make(map[string]context.CancelFunc)
	taskRecords = make(map[string]*TaskRecord)
	return n
}

// isTaskCleared 判断任务是否已被手动清除（用于抑制回调）。
func isTaskCleared(taskID string) bool {
	taskRecordsMu.Lock()
	defer taskRecordsMu.Unlock()
	return clearedTasks[taskID]
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
		strings.Contains(lower, "websocket_link is empty") ||
		strings.Contains(lower, "浏览器连接预检失败") ||
		strings.Contains(lower, "无法连接undetectable") ||
		strings.Contains(lower, "已尝试启动undetectable") ||
		strings.Contains(lower, "wait profile started timeout") ||
		strings.Contains(lower, "context deadline exceeded")
}

// ===== 任务结果回调 =====

// taskResultURL 任务完成后的回调地址（已与 account_sys 确认）。
const taskResultURL = "http://47.89.235.227:3366/api/v1/browser_tasks/result"

// machineRestartedURL 「机器重启上报」地址（与任务回调指向同一台 account_sys）。
// 本机进程启动时上报一次，account_sys 收到后会立即重置本机丢失的异步任务，
// 不必再等 check_timeout_tasks 的 45 分钟兜底窗口。
const machineRestartedURL = "http://47.89.235.227:3366/api/v1/browser_tasks/machine_restarted"

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
	// 任务已被手动清除：不再回调 account_sys，避免把「已作废」的任务状态写回去。
	if isTaskCleared(payload.TaskID) {
		logger.Print("TASK_CB", "任务已被清除，跳过回调: "+payload.TaskID)
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

// ===== 机器重启上报 =====

// notifyMachineRestarted 上报「本机进程已重启」。
//
// 背景：任务记录只存在内存里（taskRecords），进程一挂就全部丢失，重启后既没有恢复能力、
// 也无法回答 account_sys 对旧 task_id 的查询（一律 404）。account_sys 侧的
// TaskScheduler.check_timeout_tasks 虽然会用「查机器端 → 查不到就重置」兜底，
// 但它的判定窗口是 45 分钟，导致重启后任务要拖很久才会被重置、再等平台分配窗口重跑。
// 启动时主动上报一次，可把这段延迟从 45 分钟降到近乎为零。
//
// MACHINE_IP 环境变量（可选）：告知 account_sys 只处理本机上的登记记录。
// 不设置时 account_sys 会全量扫描 —— 它只重置「机器端查不到」的任务，所以同样安全，
// 只是会多打几个查询请求。建议设置。
func notifyMachineRestarted(logger *logx.Logger) {
	if machineRestartedURL == "" {
		return
	}

	payload := map[string]string{}
	if v := strings.TrimSpace(os.Getenv("MACHINE_IP")); v != "" {
		payload["machine_ip"] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Print("BOOT", "上报机器重启：构造请求体失败: "+err.Error())
		return
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		reqCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, machineRestartedURL, bytes.NewReader(body))
		if err != nil {
			cancel()
			logger.Print("BOOT", "上报机器重启：构建请求失败: "+err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", accountCheckUA)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			logger.Print("BOOT", fmt.Sprintf("上报机器重启失败(第%d次): %v", attempt, err))
		} else {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			cancel()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				logger.Print("BOOT", fmt.Sprintf("已上报机器重启(第%d次)，account_sys 响应: %s", attempt, strings.TrimSpace(string(raw))))
				return
			}
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
			logger.Print("BOOT", fmt.Sprintf("上报机器重启失败(第%d次): %v", attempt, lastErr))
		}

		if attempt < 3 {
			time.Sleep(5 * time.Second)
		}
	}
	logger.Print("BOOT", "上报机器重启最终失败（不影响服务，account_sys 仍会走 45 分钟兜底）: "+lastErr.Error())
}

// handleTaskQuery GET /tasks/{id} 查询单个任务状态（作为回调失败/重启丢任务时的补充排查手段）。
func handleTaskQuery(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		taskID := strings.TrimPrefix(r.URL.Path, "/tasks/")
		if taskID == "" || strings.Contains(taskID, "/") {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Type: "error", ErrorInfo: "task_id is required"})
			return
		}
		rec := getTaskRecord(taskID)
		if rec == nil {
			writeJSON(w, http.StatusNotFound, ErrorResponse{Type: "error", ErrorInfo: "task not found"})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

// handleTaskList GET /tasks 查询任务汇总与进度。
// 可选 query 参数：
//   - status: queued | running | success | failed（按状态过滤列表）
//   - type:   fetch | nurture | send_message | check_reply | *_publish（按类型过滤列表）
//   - limit:  列表返回条数，默认 50，最大 500
//
// 返回：total（全局任务总数）、status_counts（全局各状态计数）、type_counts（全局各类型计数）、
// matched（符合过滤条件的条数）、has_more（是否还有更多）、tasks（明细列表，按提交时间倒序）。
func handleTaskList(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		q := r.URL.Query()
		filterStatus := q.Get("status")
		filterType := q.Get("type")
		limit := 50
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
				if limit > 500 {
					limit = 500
				}
			}
		}

		taskRecordsMu.Lock()
		totalAll := len(taskRecords)
		statusCounts := map[string]int{
			taskStatusQueued:  0,
			taskStatusRunning: 0,
			taskStatusSuccess: 0,
			taskStatusFailed:  0,
		}
		typeCounts := map[string]int{}
		var matched []*TaskRecord
		for _, rec := range taskRecords {
			typeCounts[rec.Type]++
			statusCounts[rec.Status]++
			if filterStatus != "" && rec.Status != filterStatus {
				continue
			}
			if filterType != "" && rec.Type != filterType {
				continue
			}
			matched = append(matched, rec)
		}
		taskRecordsMu.Unlock()

		sort.Slice(matched, func(i, j int) bool {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		})

		hasMore := false
		if len(matched) > limit {
			matched = matched[:limit]
			hasMore = true
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"total":         totalAll,
			"status_counts": statusCounts,
			"type_counts":   typeCounts,
			"matched":       len(matched),
			"has_more":      hasMore,
			"tasks":         matched,
		})
	}
}

// handleTaskClear POST /tasks/clear 中断并清空所有任务。
// 排队中(queued)的任务会被取消、不再执行；正在运行(running)的任务会被中断（对应浏览器可能
// 来不及正常收尾而残留，需自行确认）；被清除的任务不再回调 account_sys。
func handleTaskClear(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}
		n := clearAllTasks()
		logger.Print("TASK_CLEAR", fmt.Sprintf("已清除全部任务: %d 个", n))
		writeJSON(w, http.StatusOK, map[string]any{"type": "cleared", "count": n})
	}
}
