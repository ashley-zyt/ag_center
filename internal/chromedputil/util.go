package chromedputil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// pageTargets 列出浏览器中所有的 page 标签页。
// ctx 必须是 chromedp.NewContext 创建的浏览器上下文(带有 Browser executor)。
func pageTargets(ctx context.Context) ([]*target.Info, error) {
	targets, err := chromedp.Targets(ctx)
	if err != nil {
		return nil, err
	}
	var pages []*target.Info
	for _, t := range targets {
		if t.Type == "page" && t.TargetID != "" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// browserExecutor 从 chromedp context 中取出 browser executor, 用于执行浏览器级 CDP 命令。
func browserExecutor(ctx context.Context) (cdp.Executor, error) {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Browser == nil {
		return nil, errors.New("chromedputil: context 缺少 browser executor(请传入 chromedp.NewContext 创建的上下文)")
	}
	return c.Browser, nil
}

// closePageTarget 通过 browser executor 关闭指定标签页。
func closePageTarget(ctx context.Context, exec cdp.Executor, id target.ID) error {
	closeCtx, cancelClose := context.WithTimeout(ctx, 3*time.Second)
	defer cancelClose()
	return target.CloseTarget(id).Do(cdp.WithExecutor(closeCtx, exec))
}

// CloseAllTabsThenBrowser 关闭浏览器中所有 page 标签页(供停止 profile 前清理用)。
// ctx 必须是 chromedp.NewContext 创建的浏览器上下文, 不能是 allocator 上下文。
func CloseAllTabsThenBrowser(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	pages, err := pageTargets(ctx)
	if err != nil {
		return err
	}
	if len(pages) == 0 {
		return nil
	}

	exec, err := browserExecutor(ctx)
	if err != nil {
		return err
	}

	for _, t := range pages {
		if err := closePageTarget(ctx, exec, t.TargetID); err != nil {
			return err
		}
	}
	return nil
}

// CleanExtraTabs 关闭多余的标签页，只保留第一个。
// ctx 必须是 chromedp.NewContext 创建的浏览器上下文, 不能是 allocator 上下文。
func CleanExtraTabs(ctx context.Context, logger *logx.Logger, platformTag string) {
	if ctx.Err() != nil {
		logger.Print(platformTag, "CleanExtraTabs: context 已失效: "+ctx.Err().Error())
		return
	}

	pages, err := pageTargets(ctx)
	if err != nil {
		logger.Print(platformTag, "获取标签页列表失败: "+err.Error())
		return
	}

	// 如果只有一个或没有标签页，无需清理
	if len(pages) <= 1 {
		return
	}

	logger.Print(platformTag, fmt.Sprintf("发现 %d 个标签页，清理多余的 %d 个", len(pages), len(pages)-1))

	exec, err := browserExecutor(ctx)
	if err != nil {
		logger.Print(platformTag, err.Error())
		return
	}

	// 保留第一个，关闭其余的
	for i := 1; i < len(pages); i++ {
		if err := closePageTarget(ctx, exec, pages[i].TargetID); err != nil {
			logger.Print(platformTag, "关闭标签页失败: "+err.Error())
		}
	}

	logger.Print(platformTag, "已清理多余标签页，保留1个")
}

// CloseTabsAndStopProfile 关闭所有标签页, 并请求停止 Undetectable Profile。
// browserCtx 必须是 chromedp.NewContext 创建的浏览器上下文。
func CloseTabsAndStopProfile(ctx context.Context, browserCtx context.Context, logger *logx.Logger,
	profileID, undetectableHost string, undetectablePort int, platformTag string) {

	if browserCtx != nil {
		closeCtx, cancelClose := context.WithTimeout(browserCtx, 15*time.Second)
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

// PageStallTimeout 页面停留超时默认值(30秒)
const PageStallTimeout = 30 * time.Second

// PageStallWatcher 页面停留监控器。
// 后台轮询当前页面 URL, 若同一个 URL 持续停留超过阈值则判定为卡在某个页面。
type PageStallWatcher struct {
	mu         sync.Mutex
	stalled    bool
	stalledURL string
}

// Stalled 是否检测到页面停留超时。
func (w *PageStallWatcher) Stalled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}

// URL 卡住的页面链接(仅在 Stalled 为 true 时有意义)。
func (w *PageStallWatcher) URL() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalledURL
}

func (w *PageStallWatcher) markStalled(url string) {
	w.mu.Lock()
	w.stalled = true
	w.stalledURL = url
	w.mu.Unlock()
}

// WatchPageStall 启动页面停留监控。
// parent 必须是 chromedp 浏览器上下文; 返回的 context 在检测到页面停留超时后会被取消。
// 调用方通过 watcher.Stalled()/URL() 获取是否卡住以及卡住的页面链接。
func WatchPageStall(parent context.Context, logger *logx.Logger, timeout time.Duration) (context.Context, context.CancelFunc, *PageStallWatcher) {
	if timeout <= 0 {
		timeout = PageStallTimeout
	}
	ctx, cancel := context.WithCancel(parent)
	w := &PageStallWatcher{}

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		var lastURL string
		var lastChange time.Time
		started := false

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var url string
				locCtx, locCancel := context.WithTimeout(ctx, 3*time.Second)
				err := chromedp.Run(locCtx, chromedp.Location(&url))
				locCancel()
				if err != nil {
					continue
				}
				if !started || url != lastURL {
					lastURL = url
					lastChange = time.Now()
					started = true
					continue
				}
				if time.Since(lastChange) >= timeout {
					w.markStalled(url)
					logger.Print("STALL", fmt.Sprintf("页面停留超过 %v, 判定为卡在页面: %s", timeout, url))
					cancel()
					return
				}
			}
		}
	}()

	return ctx, cancel, w
}
