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
func runPublish(ctx context.Context, logger *logx.Logger, job publishJob, run publishRunner) (string, string, startByNameResult) {
	// 1. 获取全局并发额度（排队用无 deadline 的 ctx：排队等待不计入执行超时）
	if err := acquireBrowserSlot(ctx); err != nil {
		return "failed", "获取并发额度失败: " + err.Error(), startByNameResult{}
	}
	defer releaseBrowserSlot()

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

func executeFacebookPublish(ctx context.Context, logger *logx.Logger, req FacebookPublishRequest) (string, string, startByNameResult) {
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
	})
}

func executeTwitterPublish(ctx context.Context, logger *logx.Logger, req TwitterPublishRequest) (string, string, startByNameResult) {
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
	})
}

func executeYoutubePublish(ctx context.Context, logger *logx.Logger, req YouTubePublishRequest) (string, string, startByNameResult) {
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
	})
}

func executeTiktokPublish(ctx context.Context, logger *logx.Logger, req TikTokPublishRequest) (string, string, startByNameResult) {
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
	})
}

func executeInstagramPublish(ctx context.Context, logger *logx.Logger, req InstagramPublishRequest) (string, string, startByNameResult) {
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
	})
}

// ===== 异步执行包装（handler 与重启重放共用）=====

// runAsyncPublish 异步执行一个发布任务：置 running → 执行 → finishTask → 回调。
func runAsyncPublish(ctx context.Context, logger *logx.Logger, taskID, taskType, profileName, ref string, exec func() (string, string, startByNameResult)) {
	go func() {
		setTaskState(taskID, taskStatusRunning)
		status, info, _ := exec()
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
		})
	}()
}

// ===== 重启重放 =====

// buildPublishExecutor 根据任务类型与原始请求体 payload，构建可执行的发布闭包。
// 返回 (exec, true) 表示解析成功；解析失败返回 (nil, false)。
func buildPublishExecutor(ctx context.Context, taskType, payload string, logger *logx.Logger) (func() (string, string, startByNameResult), bool) {
	switch taskType {
	case "facebook_publish":
		var req FacebookPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeFacebookPublish(ctx, logger, req)
		}), true
	case "twitter_publish":
		var req TwitterPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeTwitterPublish(ctx, logger, req)
		}), true
	case "youtube_publish":
		var req YouTubePublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeYoutubePublish(ctx, logger, req)
		}), true
	case "tiktok_publish":
		var req TikTokPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeTiktokPublish(ctx, logger, req)
		}), true
	case "instagram_publish":
		var req InstagramPublishRequest
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			return nil, false
		}
		return guard(logger, func() (string, string, startByNameResult) {
			return executeInstagramPublish(ctx, logger, req)
		}), true
	}
	return nil, false
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
		// 重新注册可取消 context，使 POST /tasks/clear 仍能中断重放的任务
		ctx, cancel := context.WithCancel(context.Background())
		taskRecordsMu.Lock()
		taskCancels[rec.TaskID] = cancel
		taskRecordsMu.Unlock()

		exec, ok := buildPublishExecutor(ctx, rec.Type, rec.Payload, logger)
		if !ok {
			finishTask(rec.TaskID, taskStatusInterrupted, "服务重启中断（无法解析任务参数）")
			logger.Print("RESUME", "重放失败（无法解析参数）: "+rec.TaskID)
			continue
		}
		logger.Print("RESUME", "重放排队中的发布任务: "+rec.TaskID+" type="+rec.Type+" profile="+rec.ProfileName)
		runAsyncPublish(ctx, logger, rec.TaskID, rec.Type, rec.ProfileName, rec.Ref, exec)
	}
}
