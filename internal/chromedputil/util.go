package chromedputil

import (
	"context"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/target"
)

func CloseAllTabsThenBrowser(ctx context.Context) error {
	targets, err := target.GetTargets().Do(ctx)
	if err != nil {
		return err
	}

	var pageTargets []*target.Info
	for _, t := range targets {
		if t.Type == "page" && t.TargetID != "" {
			pageTargets = append(pageTargets, t)
		}
	}

	for _, t := range pageTargets {
		closeCtx, cancelClose := context.WithTimeout(ctx, 3*time.Second)
		_ = target.CloseTarget(t.TargetID).Do(closeCtx)
		cancelClose()
	}

	return nil
}

func CloseTabsAndStopProfile(ctx context.Context, allocCtx context.Context, logger *logx.Logger, profileID, undetectableHost string, undetectablePort int, platformTag string) {
	if allocCtx != nil {
		closeCtx, cancelClose := context.WithTimeout(allocCtx, 10*time.Second)
		if err := CloseAllTabsThenBrowser(closeCtx); err != nil {
			logger.Print(platformTag, "关闭标签页失败: "+err.Error())
		} else {
			logger.Print(platformTag, "已关闭所有标签页")
		}
		cancelClose()
	}
	if profileID != "" && undetectableHost != "" && undetectablePort != 0 {
		stopCtx, cancelStop := context.WithTimeout(ctx, 6*time.Second)
		_ = undetectable.NewClient(undetectableHost, undetectablePort).StopProfileBestEffort(stopCtx, profileID)
		cancelStop()
		logger.Print(platformTag, "已请求停止Undetectable Profile")
	}
}
