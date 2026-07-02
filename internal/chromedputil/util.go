package chromedputil

import (
	"context"
	"time"

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
