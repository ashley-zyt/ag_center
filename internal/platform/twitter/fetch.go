package twitter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"

	"github.com/chromedp/chromedp"
)

// FetchPosts scrapes the first page of posts from X / Twitter by navigating
// to the profile page and extracting post metrics.
func FetchPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("TW_FETCH", "开始 X/Twitter 抓取流程: "+req.SourceURL)

	// 1. 打开首页链接
	logger.Print("TW_FETCH", "正在打开 X 首页")
	if err := chromedp.Run(ctx, chromedp.Navigate("https://x.com/home")); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate home failed: %w", err)
	}

	// 2. 等待个人主页链接并点击
	profileBtnSel := `a[data-testid="AppTabBar_Profile_Link"]`
	logger.Print("TW_FETCH", "等待个人主页按钮并点击")
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(profileBtnSel, chromedp.ByQuery),
		chromedp.Click(profileBtnSel, chromedp.ByQuery),
	); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("click profile link failed: %w", err)
	}

	// 3. 等待发文容器出现
	cellSel := `div[data-testid="cellInnerDiv"]`
	logger.Print("TW_FETCH", "等待发文内容加载")
	if err := chromedp.Run(ctx, chromedp.WaitVisible(cellSel, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("wait for posts failed: %w", err)
	}

	// 预留一点渲染时间
	time.Sleep(3 * time.Second)

	// 使用 JS 批量提取数据，逻辑严格遵循用户提供的选择器
	logger.Print("TW_FETCH", "正在通过 JS 提取发文数据")
	var rawPosts []struct {
		Title       string `json:"title"`
		Link        string `json:"link"`
		PublishTime string `json:"publish_time"`
		Likes       string `json:"likes"`
		Comments    string `json:"comments"`
		Shares      string `json:"shares"`
		Views       string `json:"views"`
	}

	// JS 逻辑严格遵循用户提供的选择器
	js := `(function() {
		const cells = document.querySelectorAll('div[data-testid="cellInnerDiv"]');
		const results = [];
		cells.forEach(cell => {
			const titleEl = cell.querySelector('div[data-testid="tweetText"]');
			if (!titleEl) return;

			const nameAnchor = cell.querySelector('div[data-testid="User-Name"] > a');
			const timeEl = cell.querySelector('div[data-testid="User-Name"] > a > time');
			
			const getVal = (sel) => {
				const el = cell.querySelector(sel);
				return el ? el.innerText.trim() : "";
			};

			results.push({
				title: titleEl.innerText || "",
				link: nameAnchor ? nameAnchor.href : "",
				publish_time: timeEl ? timeEl.getAttribute("datetime") : "",
				comments: getVal('button[data-testid="reply"] span'),
				shares: getVal('button[data-testid="unretweet"] span') || getVal('button[data-testid="retweet"] span'),
				likes: getVal('button[data-testid="unlike"] span') || getVal('button[data-testid="like"] span'),
				views: getVal('a[aria-label*="analytics"] span') || getVal('a[aria-label*="analytics"]')
			});
		});
		return results;
	})()`

	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &rawPosts)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("evaluate JS failed: %w", err)
	}

	// 转换数据格式
	var posts []scraper.Post
	for _, p := range rawPosts {
		posts = append(posts, scraper.Post{
			Title:       p.Title,
			Link:        p.Link,
			PublishTime: p.PublishTime,
			Likes:       parseTwitterMetric(p.Likes),
			Comments:    parseTwitterMetric(p.Comments),
			Shares:      parseTwitterMetric(p.Shares),
			Views:       parseTwitterMetric(p.Views),
		})
	}

	logger.Print("TW_FETCH", fmt.Sprintf("成功抓取到 %d 条发文", len(posts)))
	return scraper.FetchResult{Posts: posts}, nil
}

// parseTwitterMetric converts strings like "1.2K", "3M", "1,234" to integers.
func parseTwitterMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	s = strings.ReplaceAll(s, ",", "")

	multiplier := 1.0
	if strings.HasSuffix(s, "k") {
		multiplier = 1000.0
		s = strings.TrimSuffix(s, "k")
	} else if strings.HasSuffix(s, "m") {
		multiplier = 1000000.0
		s = strings.TrimSuffix(s, "m")
	} else if strings.HasSuffix(s, "b") {
		multiplier = 1000000000.0
		s = strings.TrimSuffix(s, "b")
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(val * multiplier)
}
