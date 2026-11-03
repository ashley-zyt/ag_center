package instagram

import (
	"context"

	"minimax_pro/internal/chromedputil"
)

// instagramLoggedOutTexts Instagram 退出登录页面的文案关键词。
var instagramLoggedOutTexts = []string{
	"Log into Instagram",
	"Create new account",
	"Log in with Facebook",
	"Enter your mobile number",
	"You'll need to confirm this mobile number",
	"Use another profile",
}

// checkLoggedOut 检测当前页面是否已退出登录（URL 重定向到登录页 或 页面出现登录文案）。
func checkLoggedOut(ctx context.Context) (bool, string) {
	return chromedputil.PageLoggedOut(ctx, []string{"instagram.com/accounts/"}, instagramLoggedOutTexts)
}
