package main

// 发布任务「可重放」支持 —— 让服务重启后，排队中(queued)的发布任务能自动重新入队执行，
// 而不是只能标记 interrupted 等 account_sys 重新下发。
//
// 背景：发布任务不可幂等（执行到一半被杀无法从中间恢复，盲目重跑可能重复发视频），
// 所以「执行中(running)」的任务重启后仍标记 interrupted；但「排队中(queued)」的任务
// 还没开始执行、重跑无害，只要参数完整就能自动续跑。为此：
//   - TaskRecord 增加 Payload 字段，持久化原始请求体 JSON（执行参数全在里面）；
//   - 把 5 个平台的发布执行逻辑从 handler 闭包抽成包级函数，使 HTTP 下发与重启重放共用；
//   - 重启加载时，queued 且带 payload 的发布任务保留 queued 状态，由 resumeQueuedPublishTasks
//     重新入队执行。
//
// 注意：fetch / nurture / send_message / check_reply 四类暂不支持自动重放，它们的 queued
// 任务重启后仍标记 interrupted，靠各自的调度器兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
)

// ===== 各平台发布请求结构体（从 handler 内提为包级，供重放解析 payload 使用）=====

// DingtalkNotify 任务完成后的钉钉通知配置（可选）。
// 请求里带它 = 人工/外部调用（区别于 account_sys 下发），任务完成后把结果推送到对应钉钉群。
type DingtalkNotify struct {
	Webhook string `json:"webhook"`           // 钉钉机器人 webhook 完整地址
	Keyword string `json:"keyword,omitempty"` // 机器人「自定义关键词」，消息不含时自动补齐（可选）
	Owner   string `json:"owner,omitempty"`   // 负责人名：消息里加粗显示并 @，方便对应的人直接看到（可选）
}

type FacebookPublishRequest struct {
	ProfileName      string          `json:"profile_name"`
	Title            string          `json:"title"`
	VideoOssURL      string          `json:"video_oss_url"`
	VideoPath        string          `json:"video_path"`
	Host             string          `json:"host"`
	Port             int             `json:"port"`
	WaitSeconds      int             `json:"wait_seconds"`
	UndetectablePath string          `json:"undetectable_path"`
	Async            bool            `json:"async"`
	Ref              string          `json:"ref,omitempty"`
	Batch            string          `json:"batch,omitempty"`
	BatchTotal       int             `json:"batch_total,omitempty"`
	NotifyDingtalk   *DingtalkNotify `json:"notify_dingtalk,omitempty"`
}

type TwitterPublishRequest struct {
	ProfileName      string          `json:"profile_name"`
	Text             string          `json:"text"`
	Title            string          `json:"title"`
	VideoOssURL      string          `json:"video_oss_url"`
	VideoPath        string          `json:"video_path"`
	Host             string          `json:"host"`
	Port             int             `json:"port"`
	WaitSeconds      int             `json:"wait_seconds"`
	UndetectablePath string          `json:"undetectable_path"`
	Async            bool            `json:"async"`
	Ref              string          `json:"ref,omitempty"`
	Batch            string          `json:"batch,omitempty"`
	BatchTotal       int             `json:"batch_total,omitempty"`
	NotifyDingtalk   *DingtalkNotify `json:"notify_dingtalk,omitempty"`
}

type YouTubePublishRequest struct {
	ProfileName      string          `json:"profile_name"`
	Text             string          `json:"text"`
	Title            string          `json:"title"`
	Description      string          `json:"description"`
	VideoOssURL      string          `json:"video_oss_url"`
	VideoPath        string          `json:"video_path"`
	Host             string          `json:"host"`
	Port             int             `json:"port"`
	WaitSeconds      int             `json:"wait_seconds"`
	UndetectablePath string          `json:"undetectable_path"`
	Async            bool            `json:"async"`
	Ref              string          `json:"ref,omitempty"`
	Batch            string          `json:"batch,omitempty"`
	BatchTotal       int             `json:"batch_total,omitempty"`
	NotifyDingtalk   *DingtalkNotify `json:"notify_dingtalk,omitempty"`
}

type TikTokPublishRequest struct {
	ProfileName      string          `json:"profile_name"`
	Text             string          `json:"text"`
	Title            string          `json:"title"`
	VideoOssURL      string          `json:"video_oss_url"`
	VideoPath        string          `json:"video_path"`
	Host             string          `json:"host"`
	Port             int             `json:"port"`
	WaitSeconds      int             `json:"wait_seconds"`
	UndetectablePath string          `json:"undetectable_path"`
	Async            bool            `json:"async"`
	Ref              string          `json:"ref,omitempty"`
	Batch            string          `json:"batch,omitempty"`
	BatchTotal       int             `json:"batch_total,omitempty"`
	NotifyDingtalk   *DingtalkNotify `json:"notify_dingtalk,omitempty"`
}

type InstagramPublishRequest struct {
	ProfileName      string          `json:"profile_name"`
	Text             string          `json:"text"`
	Title            string          `json:"title"`
	VideoOssURL      string          `json:"video_oss_url"`
	VideoPath        string          `json:"video_path"`
	Host             string          `json:"host"`
	Port             int             `json:"port"`
	WaitSeconds      int             `json:"wait_seconds"`
	UndetectablePath string          `json:"undetectable_path"`
	Async            bool            `json:"async"`
	Ref              string          `json:"ref,omitempty"`
	Batch            string          `json:"batch,omitempty"`
	BatchTotal       int             `json:"batch_total,omitempty"`
	NotifyDingtalk   *DingtalkNotify `json:"notify_dingtalk,omitempty"`
}

// ===== 通用发布流程 =====

// publishJob 发布任务的通用执行参数（5 个平台一致的部分）。
type publishJob struct {
	ProfileName      string
	VideoOssURL      string
	VideoPath        string
	Host             string
	Port             int
	WaitSeconds      int
	UndetectablePath string
}

// publishRunner 平台发布 + 收尾回调：给定已启动的浏览器与本地视频路径，执行平台发布，
// 并自行处理收尾（停止 profile、成功后 sleep 等），返回 (status, info)。
type publishRunner func(pubCtx context.Context, logger *logx.Logger, res startByNameResult, videoPath string) (string, string)

// runPublish 通用发布流程：并发额度 → 执行超时 → profile 锁 → 下载视频 → 启动浏览器 → 平台发布。
// 平台差异（发布函数、参数、停止时机、成功后 sleep）由 run 回调承担。
func runPublish(ctx context.Context, logger *logx.Logger, job publishJob, run publishRunner, onStart func()) (string, string, startByNameResult) {
	// 1. 获取全局并发额度（排队用无 deadline 的 ctx：排队等待不计入执行超时）
	if err := acquireBrowserSlot(ctx); err != nil {
		return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
	}
	defer releaseBrowserSlot()

	// 拿到槽位、任务真正开始执行时才置 running；排队等待期间保持 queued，
	// 这样重启后「排队中」的任务仍是 queued，能被 resumeQueuedPublishTasks 自动重放。
	if onStart != nil {
		onStart()
	}

	// 2. 拿到槽位后才起执行超时：publishTimeout 只覆盖下载/启动/发布/关闭，不含排队
	pubCtx, cancelPub := context.WithTimeout(ctx, publishTimeout)
	defer cancelPub()

	// 3. 获取 Profile 操作锁
	releaseLock := acquireProfileLock(job.ProfileName, logger)
	defer releaseLock()

	// 4. 下载视频
	var absVideoPath string
	if job.VideoOssURL != "" {
		var err error
		absVideoPath, err = downloadVideoFromOss(pubCtx, logger, job.VideoOssURL)
		if err != nil {
			return "failed", err.Error(), startByNameResult{}
		}
	} else {
		var err error
		absVideoPath, err = filepath.Abs(job.VideoPath)
		if err != nil {
			return "failed", "invalid video_path: " + err.Error(), startByNameResult{}
		}
		if _, err := os.Stat(absVideoPath); err != nil {
			return "failed", "video_path file not found", startByNameResult{}
		}
	}
	defer os.Remove(absVideoPath)

	// 5. 启动浏览器（带重试）
	res, err := startProfileByNameWithRetry(pubCtx, logger, job.ProfileName, job.Host, job.Port, job.WaitSeconds, job.UndetectablePath)
	if err != nil {
		logger.Print("E", err.Error())
		return "failed", err.Error(), startByNameResult{}
	}

	// 6. 平台发布 + 收尾
	status, info := run(pubCtx, logger, res, absVideoPath)
	return status, info, res
}

// stopProfileBestEffort 停止指定 profile（best effort，失败仅忽略）。
func stopProfileBestEffort(ctx context.Context, res startByNameResult) {
	stopCtx, cancelStop := context.WithTimeout(ctx, 6*time.Second)
	defer cancelStop()
	_ = undetectable.NewClient(res.Host, res.Port).StopProfileBestEffort(stopCtx, res.ProfileID)
}

// ===== 各平台发布执行函数 =====

func executeFacebookPublish(ctx context.Context, logger *logx.Logger, req FacebookPublishRequest, onStart func()) (string, string, startByNameResult) {
	return runPublish(ctx, logger, publishJob{
		ProfileName: req.ProfileName, VideoOssURL: req.VideoOssURL, VideoPath: req.VideoPath,
		Host: req.Host, Port: req.Port, WaitSeconds: req.WaitSeconds, UndetectablePath: req.UndetectablePath,
	}, func(pubCtx context.Context, logger *logx.Logger, res startByNameResult, videoPath string) (string, string) {
		logger.Print("FB", "开始Facebook发布流程")
		var stopOnce sync.Once
		stopProfile := func(reason string) {
			stopOnce.Do(func() {
				logger.Print("FB", "停止Profile: "+reason)
				stopProfileBestEffort(context.Background(), res)
			})
		}
		pubErr := facebook.PublishVideo(pubCtx, logger, facebook.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Title:            req.Title,
			VideoPath:        videoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		})
		if pubErr != nil {
			logger.Print("E", pubErr.Error())
			stopProfile("publish error")
			return "failed", pubErr.Error()
		}
		stopProfile("publish success")
		return "success", "publish_triggered"
	}, onStart)
}

func executeTwitterPublish(ctx context.Context, logger *logx.Logger, req TwitterPublishRequest, onStart func()) (string, string, startByNameResult) {
	return runPublish(ctx, logger, publishJob{
		ProfileName: req.ProfileName, VideoOssURL: req.VideoOssURL, VideoPath: req.VideoPath,
		Host: req.Host, Port: req.Port, WaitSeconds: req.WaitSeconds, UndetectablePath: req.UndetectablePath,
	}, func(pubCtx context.Context, logger *logx.Logger, res startByNameResult, videoPath string) (string, string) {
		logger.Print("TW", "开始Twitter发布流程")
		textToUse := req.Text
		if strings.TrimSpace(textToUse) == "" && strings.TrimSpace(req.Title) != "" {
			textToUse = req.Title
		}
		pubErr := twitter.PublishVideo(pubCtx, logger, twitter.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Text:             textToUse,
			VideoPath:        videoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		})
		if pubErr != nil {
			logger.Print("E", pubErr.Error())
			stopProfileBestEffort(pubCtx, res)
			return "failed", pubErr.Error()
		}
		stopProfileBestEffort(pubCtx, res)
		return "success", "publish_triggered"
	}, onStart)
}

func executeYoutubePublish(ctx context.Context, logger *logx.Logger, req YouTubePublishRequest, onStart func()) (string, string, startByNameResult) {
	return runPublish(ctx, logger, publishJob{
		ProfileName: req.ProfileName, VideoOssURL: req.VideoOssURL, VideoPath: req.VideoPath,
		Host: req.Host, Port: req.Port, WaitSeconds: req.WaitSeconds, UndetectablePath: req.UndetectablePath,
	}, func(pubCtx context.Context, logger *logx.Logger, res startByNameResult, videoPath string) (string, string) {
		logger.Print("YT", "开始YouTube发布流程")
		titleToUse := strings.TrimSpace(req.Title)
		if titleToUse == "" && strings.TrimSpace(req.Text) != "" {
			titleToUse = req.Text
		}
		pubErr := youtube.PublishVideo(pubCtx, logger, youtube.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Title:            titleToUse,
			Description:      req.Description,
			VideoPath:        videoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		})
		if pubErr != nil {
			logger.Print("E", pubErr.Error())
			return "failed", pubErr.Error() // YouTube 发布失败不停止 profile，维持原行为
		}
		time.Sleep(8 * time.Second)
		stopProfileBestEffort(pubCtx, res)
		return "success", "publish_triggered"
	}, onStart)
}

func executeTiktokPublish(ctx context.Context, logger *logx.Logger, req TikTokPublishRequest, onStart func()) (string, string, startByNameResult) {
	return runPublish(ctx, logger, publishJob{
		ProfileName: req.ProfileName, VideoOssURL: req.VideoOssURL, VideoPath: req.VideoPath,
		Host: req.Host, Port: req.Port, WaitSeconds: req.WaitSeconds, UndetectablePath: req.UndetectablePath,
	}, func(pubCtx context.Context, logger *logx.Logger, res startByNameResult, videoPath string) (string, string) {
		logger.Print("TT", "开始TikTok发布流程")
		textToUse := strings.TrimSpace(req.Text)
		if textToUse == "" && strings.TrimSpace(req.Title) != "" {
			textToUse = req.Title
		}
		pubErr := tiktok.PublishVideo(pubCtx, logger, tiktok.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Text:             textToUse,
			VideoPath:        videoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		})
		if pubErr != nil {
			logger.Print("E", pubErr.Error())
			return "failed", pubErr.Error() // TikTok 发布失败不停止 profile，维持原行为
		}
		time.Sleep(8 * time.Second)
		stopProfileBestEffort(pubCtx, res)
		return "success", "publish_triggered"
	}, onStart)
}

func executeInstagramPublish(ctx context.Context, logger *logx.Logger, req InstagramPublishRequest, onStart func()) (string, string, startByNameResult) {
	return runPublish(ctx, logger, publishJob{
		ProfileName: req.ProfileName, VideoOssURL: req.VideoOssURL, VideoPath: req.VideoPath,
		Host: req.Host, Port: req.Port, WaitSeconds: req.WaitSeconds, UndetectablePath: req.UndetectablePath,
	}, func(pubCtx context.Context, logger *logx.Logger, res startByNameResult, videoPath string) (string, string) {
		logger.Print("IG", "开始Instagram发布流程")
		textToUse := req.Text
		if strings.TrimSpace(textToUse) == "" && strings.TrimSpace(req.Title) != "" {
			textToUse = req.Title
		}
		pubErr := instagram.PublishVideo(pubCtx, logger, instagram.PublishRequest{
			WebsocketURL:     res.Info.WebsocketLink,
			Text:             textToUse,
			VideoPath:        videoPath,
			UndetectableHost: res.Host,
			UndetectablePort: res.Port,
			ProfileID:        res.ProfileID,
		})
		if pubErr != nil {
			logger.Print("E", pubErr.Error())
			stopProfileBestEffort(pubCtx, res)
			return "failed", pubErr.Error()
		}
		stopProfileBestEffort(pubCtx, res)
		return "success", "publish_triggered"
	}, onStart)
}

// ===== 异步执行包装（handler 与重启重放共用）=====

// runAsyncPublish 异步执行一个发布任务：执行 → 统一收尾（finalizeAsyncTask）。
// 状态置 running 的时机已下沉到执行链内部（runPublish 拿到并发槽位后经 onStart 回调触发），
// 这样排队等待槽位的任务保持 queued，重启后能被 resumeQueuedPublishTasks 自动重放。
func runAsyncPublish(ctx context.Context, logger *logx.Logger, taskID, taskType, profileName, ref string, exec func() (string, string, startByNameResult)) {
	go func() {
		status, info, _ := exec()
		finalizeAsyncTask(logger, taskID, taskType, profileName, ref, status, info, nil)
	}()
}

// ===== 重启重放 =====

// buildPublishExecutor 根据任务类型与原始请求体 payload，构建可执行的发布闭包。
// 返回 (exec, true) 表示解析成功；解析失败返回 (nil, false)。
func buildPublishExecutor(ctx context.Context, taskType, payload string, logger *logx.Logger, onStart func()) (func() (string, string, startByNameResult), bool) {
	switch taskType {
	case "facebook_publish":
		var req FacebookPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeFacebookPublish(ctx, logger, req, onStart)
		}), true
	case "twitter_publish":
		var req TwitterPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeTwitterPublish(ctx, logger, req, onStart)
		}), true
	case "youtube_publish":
		var req YouTubePublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeYoutubePublish(ctx, logger, req, onStart)
		}), true
	case "tiktok_publish":
		var req TikTokPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeTiktokPublish(ctx, logger, req, onStart)
		}), true
	case "instagram_publish":
		var req InstagramPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeInstagramPublish(ctx, logger, req, onStart)
		}), true
	}
	return nil, false
}

// claimReplay 为一次「重放」原子地占位：返回可取消 context 并登记 cancel func。
//
// 关键作用（修复重复重放）：
//   - **幂等**：若该 taskID 已有存活的 cancel（说明已有一条重放 goroutine 在跑/在排队），
//     直接返回 false，不再起第二条；
//   - **立刻把状态改写成 queued**：paused 任务被重新入队后，必须先离开 paused，
//     否则 account_sys 的 MachinePauseMonitor 每 5 分钟一轮 `GET /tasks?status=paused`
//     会再次把它当成「新暂停」而重复调 /tasks/resume，导致同一任务堆出多个 goroutine
//     一起抢并发额度；先拿到额度的那条跑完后 finishTask 会 cancel 掉最后登记的那个
//     cancel func，其余阻塞在 acquireBrowserSlot 的 goroutine 全部以
//     「获取并发额度失败: context canceled」失败，还会把已成功的记录覆盖成 failed。
func claimReplay(taskID string) (context.Context, bool) {
	taskRecordsMu.Lock()
	defer taskRecordsMu.Unlock()

	if _, alive := taskCancels[taskID]; alive {
		return nil, false // 已有存活的重放，跳过
	}
	rec, ok := taskRecords[taskID]
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	taskCancels[taskID] = cancel

	if rec.Status != taskStatusQueued {
		rec.Status = taskStatusQueued
		rec.UpdatedAt = time.Now()
		markTaskStoreDirty()
	}
	return ctx, true
}

// resumeQueuedPublishTasks 重启后把排队中的发布任务重新入队执行。
// 只处理 queued 且带 payload 的发布任务；running 与其余类型已在加载时标记 interrupted。
func resumeQueuedPublishTasks(logger *logx.Logger) {
	if !taskStoreEnabled {
		return
	}

	taskRecordsMu.Lock()
	toResume := make([]TaskRecord, 0)
	for _, rec := range taskRecords {
		if rec.Status == taskStatusQueued && rec.Payload != "" && strings.HasSuffix(rec.Type, "_publish") {
			toResume = append(toResume, *rec)
		}
	}
	taskRecordsMu.Unlock()

	for _, rec := range toResume {
		// 原子占位：幂等（已有存活重放则跳过）+ 立刻离开 paused/终态，避免被
		// MachinePauseMonitor 每 5 分钟一轮重复重放（详见 claimReplay 注释）。
		ctx, ok := claimReplay(rec.TaskID)
		if !ok {
			continue
		}

		// taskID 单独拷贝，避免闭包捕获循环变量（go1.21 for-range 变量复用）
		taskID := rec.TaskID
		exec, ok := buildPublishExecutor(ctx, rec.Type, rec.Payload, logger, func() { setTaskState(taskID, taskStatusRunning) })
		if !ok {
			finishTask(rec.TaskID, taskStatusInterrupted, "服务重启中断（无法解析任务参数）")
			logger.Print("RESUME", "重放失败（无法解析参数）: "+rec.TaskID)
			continue
		}
		logger.Print("RESUME", "重放排队中的发布任务: "+rec.TaskID+" type="+rec.Type+" profile="+rec.ProfileName)
		runAsyncPublish(ctx, logger, rec.TaskID, rec.Type, rec.ProfileName, rec.Ref, exec)
	}
}

// resumePausedPublishTasks 人工确认 Undetectable 已启动后，把 paused 的发布任务重新入队执行。
// 只处理带 payload 的发布任务（能本机重放）；非发布类的 paused 任务无 payload、无法本机重放，
// 保持 paused 不动，靠 account_sys 各自调度器兜底重发。
//
// 重新入队时会把状态立刻改成 queued（而非等拿到并发额度才变）——这样
// account_sys 的 MachinePauseMonitor 下一轮 `GET /tasks?status=paused` 就不会再看到它们，
// 不会重复触发 resume。返回值是本次真正入队的条数。
func resumePausedPublishTasks(logger *logx.Logger) int {
	taskRecordsMu.Lock()
	toResume := make([]TaskRecord, 0)
	for _, rec := range taskRecords {
		if rec.Status == taskStatusPaused && rec.Payload != "" && strings.HasSuffix(rec.Type, "_publish") {
			toResume = append(toResume, *rec)
		}
	}
	taskRecordsMu.Unlock()

	queued := 0
	for _, rec := range toResume {
		// 原子占位：幂等（已有存活重放则跳过）+ 立刻离开 paused/终态，避免被
		// MachinePauseMonitor 每 5 分钟一轮重复重放（详见 claimReplay 注释）。
		ctx, ok := claimReplay(rec.TaskID)
		if !ok {
			continue
		}
		queued++

		// taskID 单独拷贝，避免闭包捕获循环变量（go1.21 for-range 变量复用）
		taskID := rec.TaskID
		exec, ok := buildPublishExecutor(ctx, rec.Type, rec.Payload, logger, func() { setTaskState(taskID, taskStatusRunning) })
		if !ok {
			finishTask(rec.TaskID, taskStatusFailed, "恢复执行失败（无法解析任务参数）")
			logger.Print("RESUME", "恢复失败（无法解析参数）: "+rec.TaskID)
			continue
		}
		logger.Print("RESUME", "恢复暂停的发布任务: "+rec.TaskID+" type="+rec.Type+" profile="+rec.ProfileName)
		runAsyncPublish(ctx, logger, rec.TaskID, rec.Type, rec.ProfileName, rec.Ref, exec)
	}
	return queued
}

// handleTaskResume POST /tasks/resume —— 人工确认 Undetectable 已启动后，恢复被暂停的任务。
// 流程：清熔断 → 重新探测（必要时拉起）Undetectable → 成功则把 paused 发布任务重新入队。
func handleTaskResume(logger *logx.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Type: "error", ErrorInfo: "method not allowed"})
			return
		}

		// 清熔断，允许立即重新探测（人工点确认 = 明确表示 Undetectable 应已就绪）
		clearUndetectableBroken()

		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		_, _, err := ensureAPIAndMaybeStart(ctx, logger, accountDefaultHost, accountDefaultPort, 20, "")
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
				Type:      "error",
				ErrorInfo: "Undetectable 仍不可用，请先确认软件已启动再重试: " + err.Error(),
			})
			return
		}

		n := resumePausedPublishTasks(logger)
		logger.Print("RESUME", fmt.Sprintf("人工确认启动，恢复 %d 个暂停的发布任务", n))
		writeJSON(w, http.StatusOK, map[string]any{"type": "resumed", "count": n})
	}
}
