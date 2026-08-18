package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"minimax_pro/internal/logx"
)

// accountStatsBatchPayload 账号统计批量更新请求体
type accountStatsBatchPayload struct {
	Results []accountStatItem `json:"results"`
}

type accountStatItem struct {
	AccountID      int64 `json:"account_id"`
	TotalFollowers int   `json:"total_followers"`
	TotalPosts     int   `json:"total_posts"`
}

// ReportAccountStats 在采集到粉丝数和发帖数后立即调用API上报。
// 失败只打日志，不中断主流程。
// ctx用于超时控制; endpoint为空或accountID为0时直接跳过。
func ReportAccountStats(ctx context.Context, logger *logx.Logger, accountID int64, totalFollowers, totalPosts int, endpoint string) {
	if endpoint == "" || accountID == 0 {
		return
	}

	payload := accountStatsBatchPayload{
		Results: []accountStatItem{
			{AccountID: accountID, TotalFollowers: totalFollowers, TotalPosts: totalPosts},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		logger.Print("ACCT_STATS", fmt.Sprintf("序列化失败(account_id=%d): %v", accountID, err))
		return
	}

	// 使用独立的带超时context,不阻塞主流程
	apiCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(apiCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		logger.Print("ACCT_STATS", fmt.Sprintf("构建请求失败(account_id=%d): %v", accountID, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Print("ACCT_STATS", fmt.Sprintf("请求失败(account_id=%d, followers=%d, posts=%d): %v", accountID, totalFollowers, totalPosts, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		logger.Print("ACCT_STATS", fmt.Sprintf("API返回错误(account_id=%d): HTTP %d", accountID, resp.StatusCode))
		return
	}

	logger.Print("ACCT_STATS", fmt.Sprintf("账号统计上报成功(account_id=%d): followers=%d, posts=%d", accountID, totalFollowers, totalPosts))
}
