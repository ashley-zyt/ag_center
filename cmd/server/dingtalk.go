package main

// 钉钉机器人通知 —— 供「人工/外部调用」的发布任务在终态后把结果推送到指定钉钉群。
//
// 背景：account_sys 下发的任务由其自身统计/告警，机器端不额外通知；而人工通过 API 直接
// 创建的任务没有 account_sys 兜底，需要机器端把执行结果主动推到调用方指定的钉钉群。
// 因此请求体加可选字段 notify_dingtalk（webhook + keyword），带它即视为「人工调用」，
// 任务终态后发 markdown 到对应 webhook。发送为 Best Effort，失败只打日志，不影响任务状态。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"minimax_pro/internal/logx"
)

// publishTypeLabels 发布任务类型 → 展示名（用于钉钉标题）。
var publishTypeLabels = map[string]string{
	"facebook_publish":  "Facebook",
	"twitter_publish":   "X(Twitter)",
	"youtube_publish":   "YouTube",
	"tiktok_publish":    "TikTok",
	"instagram_publish": "Instagram",
}

// notifiedBatches 已发过汇总通知的批次，防止同一批多任务并发终态时重复发汇总。
// 读写都在 taskRecordsMu 锁内进行，无需单独加锁。
var notifiedBatches = make(map[string]bool)

// sendDingtalkMarkdown 发送 markdown 消息到钉钉群。
// keyword 非空时，若正文不含该关键词会自动补齐，满足钉钉「自定义关键词」安全设置。
// text 直接透传 content，不再拼「## 标题」大字号，保持消息紧凑。
func sendDingtalkMarkdown(logger *logx.Logger, webhook, keyword, title, content string) error {
	webhook = strings.TrimSpace(webhook)
	if webhook == "" {
		return fmt.Errorf("webhook 为空")
	}

	safeText := content
	if kw := strings.TrimSpace(keyword); kw != "" && !strings.Contains(safeText, kw) {
		safeText = kw + "：" + safeText
	}

	body := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"title": title,
			"text":  safeText,
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化钉钉消息失败: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("构建钉钉请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("发送钉钉失败: %w", err)
	}
	defer resp.Body.Close()

	// 钉钉即使发送失败（关键词不匹配/token 失效/限流等）也返回 HTTP 200，
	// 必须检查 body 里的 errcode（0=成功），否则会「假成功」。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var result struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &result); err == nil && result.ErrCode != 0 {
		return fmt.Errorf("钉钉拒绝: errcode=%d errmsg=%s", result.ErrCode, result.ErrMsg)
	}
	return nil
}

// notifyDingtalkResult 任务进入终态后，若该任务带了钉钉通知配置，则把结果异步推送到对应群。
// 仅 success/failed 会走到这里（paused 分支在 finalizeAsyncTask 里已提前返回，不通知）。
func notifyDingtalkResult(logger *logx.Logger, taskID, taskType, profileName, status, info string) {
	taskRecordsMu.Lock()
	batch := ""
	webhook := ""
	keyword := ""
	owner := ""
	if rec, ok := taskRecords[taskID]; ok {
		batch = rec.Batch
		webhook = rec.DingtalkWebhook
		keyword = rec.DingtalkKeyword
		owner = rec.DingtalkOwner
	}
	taskRecordsMu.Unlock()

	if webhook == "" {
		return
	}

	// 带 batch = 同一批任务：等整批全部终态后汇总发一条，避免每个任务各发一条刷屏
	if batch != "" {
		maybeSendBatchSummary(logger, batch)
		return
	}

	// 单任务：直接单发
	sendSingleResult(logger, webhook, keyword, owner, taskType, profileName, taskID, status, info)
}

// sendSingleResult 发送单条任务结果（无 batch 时用）。
func sendSingleResult(logger *logx.Logger, webhook, keyword, owner, taskType, profileName, taskID, status, info string) {
	label := publishTypeLabels[taskType]
	if label == "" {
		label = strings.TrimSuffix(taskType, "_publish")
	}
	statusLabel := "成功"
	if status != "success" {
		statusLabel = "失败"
	}
	title := fmt.Sprintf("发布结果 %s · %s", label, statusLabel)

	var b strings.Builder
	b.WriteString("发布结果")
	if owner != "" {
		b.WriteString(" **@")
		b.WriteString(owner)
		b.WriteString("**")
	}
	b.WriteString(" -- ")
	b.WriteString(profileName)
	b.WriteString("\n- ")
	b.WriteString(label)
	b.WriteString(": **")
	b.WriteString(statusLabel)
	b.WriteString("**\n")
	b.WriteString(taskID)
	b.WriteString(" [")
	b.WriteString(time.Now().Format("2006-01-02 15:04"))
	b.WriteString("]")
	if status != "success" {
		b.WriteString("\n原因：")
		b.WriteString(info)
	}
	content := b.String()

	go func() {
		if err := sendDingtalkMarkdown(logger, webhook, keyword, title, content); err != nil {
			logger.Print("DINGTALK", "钉钉通知失败: "+err.Error())
		} else {
			logger.Print("DINGTALK", "钉钉通知已发送: "+taskID)
		}
	}()
}

// batchSummaryTimeout 批次汇总的兜底窗口：即使该批声明的 batch_total 还没凑满，
// 只要距「最早一个任务进入终态」超过这个时长，也照样发汇总，避免调用方少发/传错 N 时
// 该批永远不发通知。
const batchSummaryTimeout = 30 * time.Minute

// maybeSendBatchSummary 当某批任务全部进入终态时，汇总发一条钉钉（成功/失败统计 + 失败任务 ID 列表）。
//
// 批次「发完」的判定：
//   - 该批没有任何任务声明 batch_total（都传 0）→ 沿用旧逻辑：已登记的任务全部终态即发；
//   - 有任务声明了 batch_total=N → 需「已登记数 >= N 且全部终态」才发（防止同批分多次下发时，
//     先跑完的那几个被误判成"整批已发完"而提前触发）；
//   - 兜底：无论是否凑满 N，距最早一个任务进入终态超过 batchSummaryTimeout 也发。
func maybeSendBatchSummary(logger *logx.Logger, batch string) {
	taskRecordsMu.Lock()
	if notifiedBatches[batch] {
		taskRecordsMu.Unlock()
		return
	}

	var recs []*TaskRecord
	expectedTotal := 0 // 该批声明总数（取所有任务里非零 BatchTotal 的最大值）
	for _, rec := range taskRecords {
		if rec.Batch == batch {
			recs = append(recs, rec)
			if rec.BatchTotal > expectedTotal {
				expectedTotal = rec.BatchTotal
			}
		}
	}
	if len(recs) == 0 {
		taskRecordsMu.Unlock()
		return
	}

	allDone := true
	var firstTerminal time.Time // 最早进入终态的时刻（用于兜底计时）
	for _, rec := range recs {
		if rec.Status != taskStatusSuccess && rec.Status != taskStatusFailed &&
			rec.Status != taskStatusPaused && rec.Status != taskStatusInterrupted {
			allDone = false
			continue
		}
		if firstTerminal.IsZero() || rec.UpdatedAt.Before(firstTerminal) {
			firstTerminal = rec.UpdatedAt
		}
	}

	registered := len(recs)
	// 已凑满（或未声明 batch_total 时只要全部终态）才发
	enough := allDone && (expectedTotal == 0 || registered >= expectedTotal)
	// 兜底：最早终态已超过窗口，即使没凑满也发，避免永远不发
	timeout := !firstTerminal.IsZero() && time.Since(firstTerminal) >= batchSummaryTimeout
	if !enough && !timeout {
		taskRecordsMu.Unlock()
		return
	}

	notifiedBatches[batch] = true
	webhook := recs[0].DingtalkWebhook
	keyword := recs[0].DingtalkKeyword
	owner := recs[0].DingtalkOwner

	// 浏览器名（去重排序，通常同批同浏览器）
	profileSet := make(map[string]struct{})
	// 按平台聚合成功/失败，platformOrder 保持平台首次出现顺序
	type platformAgg struct {
		success int
		failed  int
	}
	platformOrder := make([]string, 0)
	agg := make(map[string]*platformAgg)
	for _, rec := range recs {
		if rec.ProfileName != "" {
			profileSet[rec.ProfileName] = struct{}{}
		}
		label := publishTypeLabels[rec.Type]
		if label == "" {
			label = strings.TrimSuffix(rec.Type, "_publish")
		}
		a := agg[label]
		if a == nil {
			a = &platformAgg{}
			agg[label] = a
			platformOrder = append(platformOrder, label)
		}
		if rec.Status == taskStatusSuccess {
			a.success++
		} else {
			a.failed++
		}
	}
	profiles := make([]string, 0, len(profileSet))
	for p := range profileSet {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	profileLabel := strings.Join(profiles, ",")
	taskRecordsMu.Unlock()

	title := "发布结果 批次汇总"
	var b strings.Builder
	b.WriteString("发布结果")
	if owner != "" {
		b.WriteString(" **@")
		b.WriteString(owner)
		b.WriteString("**")
	}
	b.WriteString(" -- ")
	b.WriteString(profileLabel)
	b.WriteString(" [**")
	b.WriteString(batch)
	b.WriteString("**]\n")
	for i, label := range platformOrder {
		if i == 0 {
			b.WriteString("- ")
		} else {
			b.WriteString("；")
		}
		a := agg[label]
		var status string
		switch {
		case a.failed == 0:
			status = "**成功**"
		case a.success == 0:
			status = "**失败**"
		default:
			status = fmt.Sprintf("**成功 %d / 失败 %d**", a.success, a.failed)
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(status)
	}
	b.WriteString("\n")
	b.WriteString(time.Now().Format("2006-01-02 15:04"))
	content := b.String()

	go func() {
		if err := sendDingtalkMarkdown(logger, webhook, keyword, title, content); err != nil {
			logger.Print("DINGTALK", "钉钉批次汇总通知失败: "+err.Error())
		} else {
			logger.Print("DINGTALK", "钉钉批次汇总已发送: batch="+batch)
		}
	}()
}
