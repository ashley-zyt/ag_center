package twitter

import (
	"context"
	"errors"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"
)

// FetchPosts scrapes the first page of posts from the given X / Twitter
// account URL. It is currently a stub and returns an explicit error so the
// caller knows the platform-specific DOM logic is not implemented yet.
func FetchPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("TW_FETCH", "TODO: 实现 X/Twitter 发文列表抓取: "+req.SourceURL)
	return scraper.FetchResult{}, errors.New("twitter FetchPosts not implemented")
}
