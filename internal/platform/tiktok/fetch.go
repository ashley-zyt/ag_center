package tiktok

import (
	"context"
	"errors"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"
)

// FetchPosts scrapes the first page of videos from the given TikTok
// account URL. It is currently a stub and returns an explicit error so
// the caller knows the platform-specific DOM logic is not implemented yet.
func FetchPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("TT_FETCH", "TODO: 实现 TikTok 发文列表抓取: "+req.SourceURL)
	return scraper.FetchResult{}, errors.New("tiktok FetchPosts not implemented")
}
