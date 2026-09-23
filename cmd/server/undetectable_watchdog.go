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
// 本巡检（默认 30 秒一轮）在 Undetectable 可用时做两件事：
//  1. 清熔断（markUndetectableBroken 的 30 秒冷却不必再等「下一个任务」来解除）；
//  2. 自动重放 paused 的发布任务（不必再等 account_sys 那 5 分钟一轮的 resume）。
//
// 注意：这里**不再**「收尾卡死任务」。曾经用 UpdatedAt 超时来判定卡死并 finishTask(cancel)，
// 但排队等并发额度的任务 UpdatedAt 不刷新，会被误判成卡死、批量 cancel —— 表现为大量
// 「获取并发额度失败: context canceled」。各任务本有自身超时（发布 15min、采集 20min/账号、
// 养号 30min）会自行收尾，无需巡检代劳；paused 由本巡检重放，interrupted 由 account_sys 兜底。
type watchdogConfig struct {
	// 探活间隔
	interval time.Duration
	// 单次探活的超时
	probeTimeout time.Duration
	// 是否启用（UNDETECTABLE_WATCHDOG=0/false/off 可关）
	enabled bool
}

var defaultWatchdogConfig = watchdogConfig{
	interval:     30 * time.Second,
	probeTimeout: 8 * time.Second,
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

	logger.Print("WATCHDOG", "Undetectable 健康巡检已启动：每 "+cfg.interval.String()+" 探活一次")

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

	// 自动重放 paused 的发布任务（带 payload 的，能本机续跑）
	if n := resumePausedPublishTasks(logger); n > 0 {
		logger.Print("WATCHDOG", fmt.Sprintf("探活成功，自动重放暂停的发布任务: %d 条", n))
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
