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
	// taskStatusInterrupted 只出现在「重启后从快照加载出来的历史记录」上：
	// 上次进程退出时任务还没跑完，执行它的 goroutine 已消失，不会有任何东西再推进它。
	// 运行期不会主动写入这个状态。
	taskStatusInterrupted = "interrupted"
	// taskStatusPaused 任务因「Undetectable 未启动」被挂起：不回调 account_sys、不标失败，
	// 只在本机记录，等人工确认启动后通过 POST /tasks/resume 恢复执行。
	taskStatusPaused = "paused"
)

// TaskRecord 单个任务的运行记录，用于查询进度。
// 常驻内存，并通过 task_store.go 落成本地 SQLite 数据库，服务重启后仍可查询。
type TaskRecord struct {
	TaskID      string    `json:"task_id"`
	Type        string    `json:"type"`
	ProfileName string    `json:"profile_name,omitempty"`
	Ref         string    `json:"ref,omitempty"`
	Batch       string    `json:"batch,omitempty"`       // 批次标识，由调用方提交时指定（透传），用于对账「哪一批」
	BatchTotal  int       `json:"batch_total,omitempty"` // 该批声明的任务总数（可选）：用于批次汇总「等满 N 个终态才发」，避免分开发时提前触发
	Payload     string    `json:"payload,omitempty"`     // 原始请求体 JSON，供服务重启后重放 queued 的发布任务
	Status      string    `json:"status"`
	Message     string    `json:"message,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// 钉钉通知目标（可选）：仅人工/外部调用的任务会带，account_sys 下发的为空。
	// 任务进入终态（success/failed）后，若 webhook 非空则把结果推送到对应钉钉群。
	DingtalkWebhook string `json:"dingtalk_webhook,omitempty"`
	DingtalkKeyword string `json:"dingtalk_keyword,omitempty"`
	DingtalkOwner   string `json:"dingtalk_owner,omitempty"`
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
//
// batch 为调用方指定的批次标识（可空）。同一批下发的任务带上同一个 batch，之后就能用
// GET /tasks?batch=xxx 或 /tasks/summary 的 batches 聚合，精确回答"这批跑完没有"。
// batchTotal 为该批声明的任务总数（可选，0 表示不声明）：用于批次汇总「等满 N 个且全终态才发」，
// 避免同批任务分多次下发时，先跑完的那几个被误判成"整批已发完"而提前触发汇总。
// payload 为原始请求体 JSON（可空），供服务重启后重放 queued 的发布任务（其余类型暂不重放）。
// notify 为钉钉通知目标（可空）：非空表示人工/外部调用，任务终态后发结果到对应钉钉群。
func registerTask(taskType, profileName, ref, batch string, batchTotal int, payload string, notify *DingtalkNotify) (string, context.Context) {
	taskID := newTaskID()
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()

	var webhook, keyword, owner string
	if notify != nil {
		webhook = strings.TrimSpace(notify.Webhook)
		keyword = strings.TrimSpace(notify.Keyword)
		owner = strings.TrimSpace(notify.Owner)
	}

	taskRecordsMu.Lock()
	taskRecords[taskID] = &TaskRecord{
		TaskID:          taskID,
		Type:            taskType,
		ProfileName:     profileName,
		Ref:             ref,
		Batch:           strings.TrimSpace(batch),
		BatchTotal:      batchTotal,
		Payload:         payload,
		Status:          taskStatusQueued,
		CreatedAt:       now,
		UpdatedAt:       now,
		DingtalkWebhook: webhook,
		DingtalkKeyword: keyword,
		DingtalkOwner:   owner,
	}
	taskCancels[taskID] = cancel
	taskRecordsMu.Unlock()
	markTaskStoreDirty()
	return taskID, ctx
}

// setTaskState 更新任务状态（message 保持不变）。
func setTaskState(taskID, status string) {
	taskRecordsMu.Lock()
	changed := false
	if rec, ok := taskRecords[taskID]; ok && rec.Status != status {
		rec.Status = status
		rec.UpdatedAt = time.Now()
		changed = true
	}
	taskRecordsMu.Unlock()
	if changed {
		markTaskStoreDirty()
	}
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
	markTaskStoreDirty()
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
	return clearTasks(nil, nil)
}

// clearTasks 中断并清空任务。types / statuses 为 nil 时清空全部；
// 否则只清类型命中 types 且状态命中 statuses 的任务（用于「只中断某类执行中/排队中的任务」，
// 保留 success/failed 历史）。
// 被清除的任务会记入 clearedTasks 以抑制回调（异步 goroutine 结束回调前先查这个标记）。
func clearTasks(types, statuses map[string]struct{}) int {
	taskRecordsMu.Lock()
	defer taskRecordsMu.Unlock()

	toClear := make(map[string]bool)
	for id, rec := range taskRecords {
		if types != nil {
			if _, ok := types[rec.Type]; !ok {
				continue
			}
		}
		if statuses != nil {
			if _, ok := statuses[rec.Status]; !ok {
				continue
			}
		}
		toClear[id] = true
	}

	for id := range toClear {
		if cancel, ok := taskCancels[id]; ok {
			cancel()
			delete(taskCancels, id)
		}
		clearedTasks[id] = true
		delete(taskRecords, id)
	}

	if len(toClear) > 0 {
		markTaskStoreDirty() // 清空也要落盘，否则重启后被清掉的记录又回来了
	}
	return len(toClear)
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

// finalizeAsyncTask 统一异步任务的收尾：置终态 + 回调 account_sys。
//
// 特殊分支：任务因「Undetectable 未启动」失败时，不标 failed、也不回调 account_sys，
// 而是标 paused 并只在本机记录 —— 等人工确认启动后通过 POST /tasks/resume 恢复执行。
// 这样既不会污染 account_sys 的成功率/失败统计，也不会让「软件没起」被误记成「任务失败」。
func finalizeAsyncTask(logger *logx.Logger, taskID, taskType, profileName, ref, status, info string, result any) {
	if status != "success" && isUndetectableNotStarted(info) {
		finishTask(taskID, taskStatusPaused, info)
		logger.Print("TASK_PAUSE", "任务因 Undetectable 未启动而暂停，等待人工确认启动: "+taskID+" ("+info+")")
		return
	}

	finalStatus := taskStatusSuccess
	if status != "success" {
		finalStatus = taskStatusFailed
	}
	finishTask(taskID, finalStatus, info)
	callbackTaskResult(context.Background(), logger, TaskResultPayload{
		TaskID:      taskID,
		ProfileName: profileName,
		TaskType:    taskType,
		Ref:         ref,
		Status:      status,
		Message:     info,
		Result:      result,
	})
	// 钉钉通知（仅人工/外部调用带配置的任务，best effort）
	notifyDingtalkResult(logger, taskID, taskType, profileName, status, info)
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
// 背景：任务记录已用 SQLite 持久化，重启后历史记录能恢复，但未完成任务会按规则改写：
//
//	running 与「非发布的 queued」会被标记 interrupted（进程已死、不可幂等续跑），
//	只有排队中的发布任务（带 payload）会由 resumeQueuedPublishTasks 自动重放。
//
// 被标 interrupted 的任务在 account_sys 侧需要尽快重置重发，而 TaskScheduler.check_timeout_tasks
// 的判定窗口是 45 分钟，会导致重启后这些任务拖很久才被重置、再等平台分配窗口重跑。
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

// handleTaskList GET /tasks 任务总览与进度列表。
//
// 过滤参数（见 task_stats.go::parseTaskFilter）：status / type / profile_name 支持逗号分隔多值，
// ref_prefix 按前缀匹配（如 ref_prefix=Account:,WarmupTask: 只看本系统下发的任务）。
//
// 响应口径：status_counts / type_counts / type_status_counts 为**全局**统计（不受过滤影响），
// tasks 明细受过滤影响，matched 为过滤命中的总条数（limit 截断前）。
func handleTaskList(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		q := r.URL.Query()
		filter := parseTaskFilter(q)
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
		global := newTaskCounters()
		matched := make([]*TaskRecord, 0, len(taskRecords))
		for _, rec := range taskRecords {
			global.add(rec)
			if filter.match(rec) {
				matched = append(matched, rec)
			}
		}
		taskRecordsMu.Unlock()

		sort.Slice(matched, func(i, j int) bool {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		})

		// matched 表示「过滤后命中多少条」，在截断前统计 —— 调用方据此可判断真实堆积量
		// （例如 ?status=queued&limit=1 时 matched 即为排队总数）。
		matchedTotal := len(matched)

		hasMore := false
		if len(matched) > limit {
			matched = matched[:limit]
			hasMore = true
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"total":              global.Total,
			"status_counts":      global.StatusCounts,
			"source_counts":      global.SourceCounts,
			"type_counts":        global.TypeCounts,
			"type_status_counts": global.TypeStatusCounts,
			"matched":            matchedTotal,
			"returned":           len(matched),
			"has_more":           hasMore,
			"store":              taskStoreStatus(),
			"tasks":              matched,
		})
	}
}

// handleTaskClear POST /tasks/clear 中断并清空任务。
// 可选参数（query 或 JSON body，均逗号分隔多值）：
//   - type:   只清指定类型的任务，如 ?type=tiktok_publish
//   - status: 只清指定状态的任务，如 ?status=running,queued（中断执行中/排队中，保留 success/failed）
//
// 两者可组合；都不传则清空全部。
// 排队中(queued)的任务会被取消、不再执行；正在运行(running)的任务会被中断（对应浏览器可能
// 来不及正常收尾而残留，需自行确认）；被清除的任务不再回调 account_sys。
func handleTaskClear(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		typeParam := r.URL.Query().Get("type")
		statusParam := r.URL.Query().Get("status")
		if r.Body != nil {
			if data, err := io.ReadAll(r.Body); err == nil && len(data) > 0 {
				var body struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				}
				if json.Unmarshal(data, &body) == nil {
					if typeParam == "" {
						typeParam = body.Type
					}
					if statusParam == "" {
						statusParam = body.Status
					}
				}
			}
		}

		types := parseCSVSet(typeParam)
		statuses := parseCSVSet(statusParam)

		n := clearTasks(types, statuses)
		filter := ""
		if types != nil {
			filter += "类型:" + typeParam
		}
		if statuses != nil {
			if filter != "" {
				filter += " "
			}
			filter += "状态:" + statusParam
		}
		if filter != "" {
			logger.Print("TASK_CLEAR", fmt.Sprintf("已清除任务: %d 个（%s）", n, filter))
		} else {
			logger.Print("TASK_CLEAR", fmt.Sprintf("已清除全部任务: %d 个", n))
		}
		writeJSON(w, http.StatusOK, map[string]any{"type": "cleared", "count": n})
	}
}
