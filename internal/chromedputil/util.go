package chromedputil

import (
	"context"
	"sync"
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

	if len(pageTargets) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	tabsToClose := len(pageTargets)
	if tabsToClose > 1 {
		tabsToClose = len(pageTargets) - 1
	}

	for i := 0; i < tabsToClose; i++ {
		t := pageTargets[i]
		wg.Add(1)
		go func(targetID target.ID) {
			defer wg.Done()
			closeCtx, cancelClose := context.WithTimeout(ctx, 3*time.Second)
			defer cancelClose()
			_ = target.CloseTarget(targetID).Do(closeCtx)
		}(t.TargetID)
	}

	wg.Wait()
	return nil
}

func CloseTabsAndStopProfile(ctx context.Context, allocCtx context.Context, logger *logx.Logger,
	profileID, undetectableHost string, undetectablePort int, platformTag string) {

	if allocCtx != nil {
		closeCtx, cancelClose := context.WithTimeout(allocCtx, 5*time.Second)
		if err := CloseAllTabsThenBrowser(closeCtx); err != nil {
			logger.Print(platformTag, "清理标签页遇到异常 (Best Effort): "+err.Error())
		} else {
			logger.Print(platformTag, "已清理冗余标签页")
		}
		cancelClose()
	}

	if profileID != "" && undetectableHost != "" && undetectablePort != 0 {
		stopCtx, cancelStop := context.WithTimeout(ctx, 10*time.Second)
		err := undetectable.NewClient(undetectableHost, undetectablePort).StopProfileBestEffort(stopCtx, profileID)
		cancelStop()

		if err != nil {
			logger.Print(platformTag, "请求停止 Undetectable Profile 失败: "+err.Error())
		} else {
			logger.Print(platformTag, "已成功请求停止 Undetectable Profile")
			time.Sleep(3 * time.Second)
			logger.Print(platformTag, "云端同步缓冲完成，配置安全关闭")
		}
	}
}
