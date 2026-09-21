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

// sendDingtalkMarkdown 发送 markdown 消息到钉钉群。
// keyword 非空时，若标题不含该关键词会自动补齐，满足钉钉「自定义关键词」安全设置。
func sendDingtalkMarkdown(logger *logx.Logger, webhook, keyword, title, content string) error {
	webhook = strings.TrimSpace(webhook)
	if webhook == "" {
		return fmt.Errorf("webhook 为空")
	}

	safeTitle := title
	if kw := strings.TrimSpace(keyword); kw != "" && !strings.Contains(safeTitle, kw) {
		safeTitle = kw + "：" + safeTitle
	}

	body := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"title": safeTitle,
			"text":  "## " + safeTitle + "\n\n" + content,
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
func notifyDingtalkResult(logger *logx.Logger, taskID, taskType, profileName, ref, status, info string) {
	taskRecordsMu.Lock()
	webhook := ""
	keyword := ""
	owner := ""
	if rec, ok := taskRecords[taskID]; ok {
		webhook = rec.DingtalkWebhook
		keyword = rec.DingtalkKeyword
		owner = rec.DingtalkOwner
	}
	taskRecordsMu.Unlock()

	if webhook == "" {
		return
	}

	label := publishTypeLabels[taskType]
	if label == "" {
		label = strings.TrimSuffix(taskType, "_publish")
	}
	statusLabel := "成功"
	if status != "success" {
		statusLabel = "失败"
	}
	title := fmt.Sprintf("[发布结果] %s发布 · %s", label, statusLabel)

	// 正文：负责人 @ + 加粗；结果状态加粗，方便对应的人一眼看到
	var b strings.Builder
	if owner != "" {
		b.WriteString("@")
		b.WriteString(owner)
		b.WriteString("\n\n**负责人：")
		b.WriteString(owner)
		b.WriteString("**\n\n")
	}
	b.WriteString("- 任务：")
	b.WriteString(taskID)
	b.WriteString("\n- 浏览器：")
	b.WriteString(profileName)
	b.WriteString("\n- 业务标识：")
	b.WriteString(ref)
	b.WriteString("\n- 结果：**")
	b.WriteString(statusLabel)
	b.WriteString("**\n- 详情：")
	b.WriteString(info)
	b.WriteString("\n- 时间：")
	b.WriteString(time.Now().Format("2006-01-02 15:04"))
	content := b.String()

	// 异步发送，不阻塞任务收尾
	go func() {
		if err := sendDingtalkMarkdown(logger, webhook, keyword, title, content); err != nil {
			logger.Print("DINGTALK", "钉钉通知失败: "+err.Error())
		} else {
			logger.Print("DINGTALK", "钉钉通知已发送: "+taskID)
		}
	}()
}
