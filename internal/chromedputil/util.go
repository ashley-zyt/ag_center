package chromedputil

import (
	"context"
	"fmt"
	"sync"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/target"
)

func CloseAllTabsThenBrowser(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

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
			if ctx.Err() != nil {
				return
			}
			closeCtx, cancelClose := context.WithTimeout(ctx, 3*time.Second)
			defer cancelClose()
			_ = target.CloseTarget(targetID).Do(closeCtx)
		}(t.TargetID)
	}

	wg.Wait()
	return nil
}

// CleanExtraTabs 关闭多余的标签页，只保留一个
// ctx 必须是一个 chromedp 浏览器上下文（由 chromedp.NewContext 创建），不能是 allocator 上下文
func CleanExtraTabs(ctx context.Context, logger *logx.Logger, platformTag string) {
	// 检查 context 是否有效
	if ctx.Err() != nil {
		logger.Print(platformTag, "CleanExtraTabs: context 已失效: "+ctx.Err().Error())
		return
	}

	targets, err := target.GetTargets().Do(ctx)
	if err != nil {
		logger.Print(platformTag, "获取标签页列表失败: "+err.Error())
		return
	}

	var pageTargets []*target.Info
	for _, t := range targets {
		if t.Type == "page" && t.TargetID != "" {
			pageTargets = append(pageTargets, t)
		}
	}

	// 如果只有一个或没有标签页，无需清理
	if len(pageTargets) <= 1 {
		return
	}

	logger.Print(platformTag, fmt.Sprintf("发现 %d 个标签页，清理多余的 %d 个", len(pageTargets), len(pageTargets)-1))

	// 保留第一个，关闭其余的
	for i := 1; i < len(pageTargets); i++ {
		t := pageTargets[i]
		closeCtx, cancelClose := context.WithTimeout(ctx, 3*time.Second)
		if err := target.CloseTarget(t.TargetID).Do(closeCtx); err != nil {
			logger.Print(platformTag, "关闭标签页失败: "+err.Error())
		}
		cancelClose()
	}

	logger.Print(platformTag, "已清理多余标签页，保留1个")
}

func CloseTabsAndStopProfile(ctx context.Context, allocCtx context.Context, logger *logx.Logger,
	profileID, undetectableHost string, undetectablePort int, platformTag string) {

	if allocCtx != nil {
		closeCtx, cancelClose := context.WithTimeout(allocCtx, 15*time.Second)
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
