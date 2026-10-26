package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"minimax_pro/internal/chromedputil"
	"minimax_pro/internal/config"
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

// undetectableMainProcName Undetectable 主程序进程名，用于状态判断与拉起前清理残留。
// 可通过环境变量 UNDETECTABLE_MAIN_PROC 覆盖。
var undetectableMainProcName = "Undetectable.exe"

// Undetectable 状态告警配置（环境变量）与告警状态追踪。
var (
	undetectableAlertWebhook  = ""
	undetectableAlertKeyword  = ""
	undetectableAlertCooldown = 10 * time.Minute

	undetectableAlertMu sync.Mutex
	lastAlertState      undetectableState
	lastAlertAt         time.Time
)

func init() {
	if v := config.Get("UNDETECTABLE_MAIN_PROC", ""); v != "" {
		undetectableMainProcName = v
	}
	if v := config.Get("UNDETECTABLE_ALERT_WEBHOOK", ""); v != "" {
		undetectableAlertWebhook = v
	}
	if v := config.Get("UNDETECTABLE_ALERT_KEYWORD", ""); v != "" {
		undetectableAlertKeyword = v
	}
	if n := config.GetInt("UNDETECTABLE_ALERT_COOLDOWN", 0); n > 0 {
		undetectableAlertCooldown = time.Duration(n) * time.Second
	}
}

// undetectableState Undetectable 的运行状态。
type undetectableState int

const (
	stateUnknown     undetectableState = iota
	stateNormal                        // /status 可用（已登录且运行）
	stateNotLoggedIn                   // /status 不可用，但主程序进程仍在（疑似未登录，API 未监听）
	stateNotRunning                    // /status 不可用，且主程序进程不存在（未启动/卡退/崩溃）
)

func (s undetectableState) String() string {
	switch s {
	case stateNormal:
		return "normal"
	case stateNotLoggedIn:
		return "not_logged_in"
	case stateNotRunning:
		return "not_running"
	default:
		return "unknown"
	}
}

// probeUndetectableState 探测 Undetectable 当前状态：
// 先看 /status 是否可用；不可用时靠「主程序进程是否存在」区分「未登录」与「未启动」。
// 依据实测：Undetectable 只要未正常登录启动，25325 端口的本地 API 都不会监听，/status 直接连接拒绝。
func probeUndetectableState(logger *logx.Logger, timeout time.Duration) undetectableState {
	probeCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := undetectable.NewClient(accountDefaultHost, accountDefaultPort)
	if err := client.Status(probeCtx); err == nil {
		return stateNormal
	}

	if chromedputil.IsProcessRunning(undetectableMainProcName) {
		logger.Print("WATCHDOG", "Undetectable API 不可用但主程序进程存在，疑似未登录")
		return stateNotLoggedIn
	}
	return stateNotRunning
}

// watchdogEnvDisabled 读取环境变量开关（未设置/无法识别时启用）。
func watchdogEnvDisabled() bool {
	v := strings.ToLower(config.Get("UNDETECTABLE_WATCHDOG", ""))
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

// watchdogTick 一轮巡检：探测状态 → 正常则清熔断+重放 paused；未运行则自动拉起；未登录则告警。
func watchdogTick(logger *logx.Logger, cfg watchdogConfig) {
	state := probeUndetectableState(logger, cfg.probeTimeout)

	switch state {
	case stateNormal:
		// 探活成功：解除熔断，让任务入口立刻恢复正常判定
		clearUndetectableBroken()
		// 自动重放 paused 的发布任务（带 payload 的，能本机续跑）
		if n := resumePausedPublishTasks(logger); n > 0 {
			logger.Print("WATCHDOG", fmt.Sprintf("探活成功，自动重放暂停的发布任务: %d 条", n))
		}
		maybeAlertUndetectable(logger, stateNormal)
	case stateNotRunning:
		// 未启动/卡退：尝试自动拉起（若配置了路径）
		path := resolveUndetectablePath("")
		if path != "" {
			logger.Print("WATCHDOG", "检测到 Undetectable 未运行，尝试自动拉起")
			if err := tryStartUndetectable(context.Background(), logger, path); err != nil {
				logger.Print("WATCHDOG", "自动拉起 Undetectable 失败: "+err.Error())
			}
		} else {
			logger.Print("WATCHDOG", "检测到 Undetectable 未运行，但未配置自动拉起路径（UNDETECTABLE_EXE）")
		}
		maybeAlertUndetectable(logger, stateNotRunning)
	case stateNotLoggedIn:
		// 主程序进程在但 API 不可用：多为「进程残留导致未正常自动登录」。
		// 实测 kill 残留进程后重新启动 Undetectable 即可恢复，因此这里也自动 kill + 拉起
		// （tryStartUndetectable 内部会先清理残留进程再启动）。
		path := resolveUndetectablePath("")
		if path != "" {
			logger.Print("WATCHDOG", "Undetectable 疑似未登录/进程残留，尝试 kill 残留并重新拉起")
			if err := tryStartUndetectable(context.Background(), logger, path); err != nil {
				logger.Print("WATCHDOG", "自动拉起 Undetectable 失败: "+err.Error())
			}
		} else {
			logger.Print("WATCHDOG", "Undetectable 疑似未登录/进程残留，但未配置自动拉起路径（UNDETECTABLE_EXE），需人工处理")
		}
		maybeAlertUndetectable(logger, stateNotLoggedIn)
	}
}

// maybeAlertUndetectable 根据状态变化发送钉钉告警（配了 UNDETECTABLE_ALERT_WEBHOOK 才发）。
// 防轰炸：状态从 normal 变异常立即告警；异常持续期间按冷却期（默认 10 分钟）最多告警一次；
// 状态恢复 normal 时发一条「已恢复」。
func maybeAlertUndetectable(logger *logx.Logger, state undetectableState) {
	if undetectableAlertWebhook == "" {
		return // 未配置告警 webhook，静默
	}

	undetectableAlertMu.Lock()
	defer undetectableAlertMu.Unlock()

	now := time.Now()

	if state == stateNormal {
		// 从异常恢复：发恢复通知并重置追踪
		if lastAlertState != stateUnknown && lastAlertState != stateNormal {
			lastAlertState = stateNormal
			lastAlertAt = now
			go sendUndetectableAlert(logger, stateNormal, "Undetectable 已恢复正常")
		}
		return
	}

	// 异常状态：状态变化立即告警，持续异常按冷却期告警
	if state == lastAlertState && now.Sub(lastAlertAt) < undetectableAlertCooldown {
		return
	}
	lastAlertState = state
	lastAlertAt = now

	detail := ""
	switch state {
	case stateNotLoggedIn:
		detail = "主程序进程存在但本地 API 不可用（疑似未登录或进程残留），已尝试自动重启，若仍异常请人工登录"
	case stateNotRunning:
		detail = "主程序未运行或已卡退"
	}
	go sendUndetectableAlert(logger, state, detail)
}

// sendUndetectableAlert 发送一条 Undetectable 状态告警到钉钉（Best Effort）。
func sendUndetectableAlert(logger *logx.Logger, state undetectableState, detail string) {
	title := fmt.Sprintf("【Undetectable %s】", state.String())
	content := fmt.Sprintf("### Undetectable 状态告警\n\n- 状态：%s\n- 详情：%s\n- 时间：%s",
		state.String(), detail, time.Now().Format("2006-01-02 15:04:05"))
	if v := config.Get("MACHINE_IP", ""); v != "" {
		content += "\n- 机器：" + v
	}
	if err := sendDingtalkMarkdown(logger, undetectableAlertWebhook, undetectableAlertKeyword, title, content); err != nil {
		logger.Print("WATCHDOG", "钉钉告警发送失败: "+err.Error())
	} else {
		logger.Print("WATCHDOG", "钉钉告警已发送: "+state.String())
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
