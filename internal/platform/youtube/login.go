package youtube

import (
	"context"

	"minimax_pro/internal/chromedputil"
)

// youtubeLoggedOutTexts YouTube 退出登录页面的文案关键词。
var youtubeLoggedOutTexts = []string{
	"Choose an account",
	"Verify it's you",
	"available to other Google apps",
}

// checkLoggedOut 检测当前页面是否已退出登录（URL 跳转到 accounts.google.com 或 页面出现登录文案）。
func checkLoggedOut(ctx context.Context) (bool, string) {
	return chromedputil.PageLoggedOut(ctx, []string{"accounts.google.com"}, youtubeLoggedOutTexts)
}
