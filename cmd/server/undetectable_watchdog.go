package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"
)

// ===== Undetectable 后台健康巡检 =====
//
// 背景（本文件要解决的问题）：
//
//	ag_center 进程内没有任何后台探活 —— 探测 Undetectable 的 ensureAPIAndMaybeStart
//	只在「任务入口」被调用。于是 Undetectable 退出又被自动拉起后：
//	  · 新进来的任务能感知（入口重新探测、成功即清熔断），所以「任务还能跑」；
//	  · 但**已经卡住的那批**没人推进：running 的要等自身超时（发布 15min、采集 20min、
//	    养号 30min）才收尾，paused 的要等 account_sys 的 MachinePauseMonitor 5 分钟
//	    一轮来救。这段空窗期里任务占着并发额度与 profile 锁，后面的全堆在 queued。
//	表现为「程序识别不到 Undetectable 回来了，只能人工重启 ag_center」。
//
// 本巡检（默认 30 秒一轮）在 Undetectable 可用时做三件事：
//  1. 清熔断（markUndetectableBroken 的 30 秒冷却不必再等「下一个任务」来解除）；
//  2. 自动重放 paused 的发布任务（不必再等 account_sys 那 5 分钟一轮的 resume）；
//  3. 收尾「卡死超阈值」的 running/queued 任务：
//     · 发布类且 payload 完整 → 重放（同一条不会重复：已有存活重放会被 claimReplay 拦下）
//     · 其余（采集/养号/私信/查回复、或 payload 缺失）→ 标 interrupted 并回调 account_sys，
//     让 account_sys 立即重置重发（等价于人工「重跑丢失任务」，但自动且幂等）
//
// 安全边界（刻意保守，避免误伤正常长任务）：
//
//	· 只在「探活成功」时清理，避免 Undetectable 真的没起时把在跑的任务全打断；
//	· 卡死阈值取得远大于各任务自身超时（默认 30 分钟 > publish 15min / nurture 30min），
//	  正常执行中的任务不会被误判 —— 它自己会先超时收尾并离开 running。
type watchdogConfig struct {
	// 探活间隔
	interval time.Duration
	// 单次探活的超时
	probeTimeout time.Duration
	// running/queued 超过该时长仍未被自身超时收尾，判定为「卡死」
	stuckAfter time.Duration
	// 是否启用（UNDETECTABLE_WATCHDOG=0/false/off 可关）
	enabled bool
}

var defaultWatchdogConfig = watchdogConfig{
	interval:     30 * time.Second,
	probeTimeout: 8 * time.Second,
	stuckAfter:   30 * time.Minute,
	enabled:      true,
}

// watchdogEnvDisabled 读取环境变量开关（未设置/无法识别时启用）。
func watchdogEnvDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("UNDETECTABLE_WATCHDOG")))
	switch v {
	case "0", "false", "off", "no", "disabled":
		return true
	}
	return false
}

// startUndetectableWatchdog 启动后台巡检 goroutine（进程生命周期内常驻）。
func startUndetectableWatchdog(ctx context.Context, logger *logx.Logger) {
	cfg := defaultWatchdogConfig
	if watchdogEnvDisabled() {
		logger.Print("WATCHDOG", "Undetectable 健康巡检已关闭（UNDETECTABLE_WATCHDOG=0），paused/卡死任务将依赖 account_sys 兜底")
		return
	}

	logger.Print("WATCHDOG", "Undetectable 健康巡检已启动：每 "+cfg.interval.String()+" 探活一次，卡死阈值 "+cfg.stuckAfter.String())

	go func() {
		ticker := time.NewTicker(cfg.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 单轮 panic 不能拖垮巡检（与任务执行侧同样的隔离思路）
				func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Print("WATCHDOG", "巡检单轮 panic 已隔离: "+panicString(r))
						}
					}()
					watchdogTick(logger, cfg)
				}()
			}
		}
	}()
}

// watchdogTick 一轮巡检：探活 → 可用则清熔断 + 重放 paused + 收尾卡死任务。
func watchdogTick(logger *logx.Logger, cfg watchdogConfig) {
	probeCtx, cancel := context.WithTimeout(context.Background(), cfg.probeTimeout)
	defer cancel()

	client := undetectable.NewClient(accountDefaultHost, accountDefaultPort)
	if err := client.Status(probeCtx); err != nil {
		// 不可用属正常状态（熔断/未启动），静默即可：任务入口会负责暂停与提示。
		return
	}

	// 探活成功：解除熔断，让任务入口立刻恢复正常判定
	clearUndetectableBroken()

	// ① 重放 paused 的发布任务（带 payload 的，能本机续跑）
	if n := resumePausedPublishTasks(logger); n > 0 {
		logger.Print("WATCHDOG", fmt.Sprintf("探活成功，自动重放暂停的发布任务: %d 条", n))
	}

	// ② 收尾卡死超阈值的 running/queued 任务
	recoverStuckTasks(logger, cfg)
}

// recoverStuckTasks 处理「执行/排队时间明显超过自身超时」的任务。
//
// 两类处理：
//   - 发布类且 payload 完整：重新入队（claimReplay 保证同一条不会堆出多条 goroutine；
//     若它其实还活着，claimReplay 会直接返回 false，我们不改状态）；
//   - 其余：标 interrupted + 回调 account_sys，让 account_sys 立刻重置重发。
func recoverStuckTasks(logger *logx.Logger, cfg watchdogConfig) {
	if !taskStoreEnabled {
		return
	}

	now := time.Now()

	// 先快照出候选（锁内只读），锁外再逐个处理，避免长时间持锁
	type cand struct {
		taskID      string
		taskType    string
		profileName string
		ref         string
		status      string
		payload     string
	}
	cands := make([]cand, 0)

	taskRecordsMu.Lock()
	for id, rec := range taskRecords {
		if rec.Status != taskStatusRunning && rec.Status != taskStatusQueued {
			continue
		}
		if rec.UpdatedAt.IsZero() || now.Sub(rec.UpdatedAt) < cfg.stuckAfter {
			continue
		}
		// 已被 clear 过的不再处理（避免把「已作废」的任务又回调回去）
		if clearedTasks[id] {
			continue
		}
		cands = append(cands, cand{
			taskID:      id,
			taskType:    rec.Type,
			profileName: rec.ProfileName,
			ref:         rec.Ref,
			status:      rec.Status,
			payload:     rec.Payload,
		})
	}
	taskRecordsMu.Unlock()

	if len(cands) == 0 {
		return
	}

	for _, c := range cands {
		isPublish := strings.HasSuffix(c.taskType, "_publish") && c.payload != ""

		// 重新读一次最新状态：快照与处理之间，任务可能已自行收尾
		if cur := getTaskState(c.taskID); cur != taskStatusRunning && cur != taskStatusQueued {
			continue
		}

		if isPublish {
			ctx, ok := claimReplay(c.taskID)
			if !ok {
				// 已有存活的重放 goroutine 在跑/在排队 —— 说明它没死，只是慢，不动它
				continue
			}
			taskID := c.taskID
			exec, built := buildPublishExecutor(ctx, c.taskType, c.payload, logger,
				func() { setTaskState(taskID, taskStatusRunning) })
			if !built {
				finishTask(c.taskID, taskStatusInterrupted, "任务卡死且无法解析参数，已中断（等待重新下发）")
				callbackTaskResult(context.Background(), logger, TaskResultPayload{
					TaskID:      c.taskID,
					ProfileName: c.profileName,
					TaskType:    c.taskType,
					Ref:         c.ref,
					Status:      "failed",
					Message:     "任务卡死超过 " + cfg.stuckAfter.String() + "，且无法解析参数，已中断等待重新下发",
				})
				logger.Print("WATCHDOG", "卡死任务无法解析参数，已中断: "+c.taskID)
				continue
			}
			logger.Print("WATCHDOG", "卡死发布任务重新入队: "+c.taskID+" type="+c.taskType+
				" profile="+c.profileName+"(原状态="+c.status+")")
			runAsyncPublish(ctx, logger, c.taskID, c.taskType, c.profileName, c.ref, exec)
			continue
		}

		// 非发布类（或 payload 缺失）：机器端无法幂等重放，交回 account_sys 重发
		msg := "机器端任务执行超时未收尾（超过 " + cfg.stuckAfter.String() + "），已中断等待重新下发"
		finishTask(c.taskID, taskStatusInterrupted, msg)
		callbackTaskResult(context.Background(), logger, TaskResultPayload{
			TaskID:      c.taskID,
			ProfileName: c.profileName,
			TaskType:    c.taskType,
			Ref:         c.ref,
			Status:      "failed",
			Message:     msg,
		})
		logger.Print("WATCHDOG", "卡死任务已中断并回调 account_sys: "+c.taskID+" type="+c.taskType)
	}
}

// panicString 把 recover 出来的值转成可读字符串（避免直接依赖 fmt 的格式细节）。
func panicString(r any) string {
	switch v := r.(type) {
	case string:
		return v
	case error:
		return v.Error()
	default:
		return "unknown panic"
	}
}
