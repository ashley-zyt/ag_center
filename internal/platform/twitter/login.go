package twitter

import (
	"context"

	"minimax_pro/internal/chromedputil"
)

// twitterLoggedOutTexts Twitter 退出登录页面的文案关键词。
var twitterLoggedOutTexts = []string{
	"See what's happening",
	"Continue with Google",
}

// twitterLoggedOutURLs Twitter 退出登录/重定向相关的 URL 关键词。
var twitterLoggedOutURLs = []string{
	"redirect_after_login",
	"x.com/login",
	"twitter.com/login",
}

// checkLoggedOut 检测当前页面是否已退出登录（URL 重定向到登录页 或 页面出现登录文案）。
func checkLoggedOut(ctx context.Context) (bool, string) {
	return chromedputil.PageLoggedOut(ctx, twitterLoggedOutURLs, twitterLoggedOutTexts)
}
