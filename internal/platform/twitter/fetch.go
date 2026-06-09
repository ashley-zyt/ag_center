package twitter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
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

	// 确认跳转
	var currentURL string
	_ = chromedp.Run(ctx, chromedp.Location(&currentURL))
	logger.Print("TW_FETCH", "当前页面 URL: "+currentURL)

	// 3. 等待发文容器出现
	cellSel := `div[data-testid="cellInnerDiv"]`
	logger.Print("TW_FETCH", "等待发文内容加载 (最长等待 30s)")
	if err := chromedp.Run(ctx, chromedp.WaitVisible(cellSel, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("wait for posts failed (possible account not logged in or profile page didn't load): %w", err)
	}

	// 预留一点渲染时间
	time.Sleep(3 * time.Second)

	// 使用原生 chromedp Actions 提取数据
	logger.Print("TW_FETCH", "正在使用原生 chromedp 提取发文数据")

	var cellNodes []*cdp.Node
	if err := chromedp.Run(ctx, chromedp.Nodes(cellSel, &cellNodes, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("get cell nodes failed: %w", err)
	}

	var posts []scraper.Post
	for i, node := range cellNodes {
		// 打印每一条发文的 Dom 结构 (OuterHTML)
		var outerHTML string
		_ = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			outerHTML, err = dom.GetOuterHTML().WithNodeID(node.NodeID).Do(ctx)
			return err
		}))
		logger.Print("TW_DOM", fmt.Sprintf("发文 [%d] DOM 结构: %s", i, outerHTML))

		// 每个 cellInnerDiv 内部进行字段提取
		var title, link, publishTime, likes, comments, shares, views string

		// 提取标题 (tweetText)
		_ = chromedp.Run(ctx, chromedp.Text(`div[data-testid="tweetText"]`, &title, chromedp.ByQuery, chromedp.FromNode(node)))
		if title == "" {
			continue // 排除非发文内容（如推荐、广告等）
		}

		// 提取链接和发布时间
		_ = chromedp.Run(ctx, chromedp.AttributeValue(`div[data-testid="User-Name"] > a`, "href", &link, nil, chromedp.ByQuery, chromedp.FromNode(node)))
		_ = chromedp.Run(ctx, chromedp.AttributeValue(`div[data-testid="User-Name"] > a > time`, "datetime", &publishTime, nil, chromedp.ByQuery, chromedp.FromNode(node)))

		// 提取互动数据
		// 注意：X 的数据可能在 span 文本里，也可能只在按钮的 aria-label 里
		_ = chromedp.Run(ctx, chromedp.Text(`button[data-testid="reply"] span`, &comments, chromedp.ByQuery, chromedp.FromNode(node)))
		if comments == "" {
			_ = chromedp.Run(ctx, chromedp.AttributeValue(`button[data-testid="reply"]`, "aria-label", &comments, nil, chromedp.ByQuery, chromedp.FromNode(node)))
		}

		// 转发（处理已转发和未转发两种状态）
		_ = chromedp.Run(ctx, chromedp.Text(`button[data-testid="unretweet"] span`, &shares, chromedp.ByQuery, chromedp.FromNode(node)))
		if shares == "" {
			_ = chromedp.Run(ctx, chromedp.Text(`button[data-testid="retweet"] span`, &shares, chromedp.ByQuery, chromedp.FromNode(node)))
		}
		if shares == "" {
			_ = chromedp.Run(ctx, chromedp.AttributeValue(`button[data-testid="retweet"]`, "aria-label", &shares, nil, chromedp.ByQuery, chromedp.FromNode(node)))
			if shares == "" {
				_ = chromedp.Run(ctx, chromedp.AttributeValue(`button[data-testid="unretweet"]`, "aria-label", &shares, nil, chromedp.ByQuery, chromedp.FromNode(node)))
			}
		}

		// 点赞
		_ = chromedp.Run(ctx, chromedp.Text(`button[data-testid="unlike"] span`, &likes, chromedp.ByQuery, chromedp.FromNode(node)))
		if likes == "" {
			_ = chromedp.Run(ctx, chromedp.Text(`button[data-testid="like"] span`, &likes, chromedp.ByQuery, chromedp.FromNode(node)))
		}
		if likes == "" {
			_ = chromedp.Run(ctx, chromedp.AttributeValue(`button[data-testid="like"]`, "aria-label", &likes, nil, chromedp.ByQuery, chromedp.FromNode(node)))
			if likes == "" {
				_ = chromedp.Run(ctx, chromedp.AttributeValue(`button[data-testid="unlike"]`, "aria-label", &likes, nil, chromedp.ByQuery, chromedp.FromNode(node)))
			}
		}

		// 观看量
		_ = chromedp.Run(ctx, chromedp.Text(`a[aria-label*="analytics"] span`, &views, chromedp.ByQuery, chromedp.FromNode(node)))
		if views == "" {
			_ = chromedp.Run(ctx, chromedp.AttributeValue(`a[aria-label*="analytics"]`, "aria-label", &views, nil, chromedp.ByQuery, chromedp.FromNode(node)))
		}

		if i == 0 {
			logger.Print("TW_DEBUG", fmt.Sprintf("原生样本 - 标题: %s, 时间: %s, 评论: %s, 转发: %s, 点赞: %s, 观看: %s",
				truncate(title, 20), publishTime, comments, shares, likes, views))
		}

		posts = append(posts, scraper.Post{
			Title:       title,
			Link:        link,
			PublishTime: publishTime,
			Likes:       parseTwitterMetric(likes),
			Comments:    parseTwitterMetric(comments),
			Shares:      parseTwitterMetric(shares),
			Views:       parseTwitterMetric(views),
		})
	}

	logger.Print("TW_FETCH", fmt.Sprintf("成功抓取到 %d 条发文", len(posts)))
	return scraper.FetchResult{Posts: posts}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// parseTwitterMetric converts strings like "1.2K", "3M", "1,234" to integers.
func parseTwitterMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	// 移除逗号
	s = strings.ReplaceAll(s, ",", "")

	// 处理 aria-label 可能带入的非数字字符（保留数字、点和单位）
	// 例如 "123 replies" -> "123"
	var clean strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || r == 'k' || r == 'm' || r == 'b' {
			clean.WriteRune(r)
		} else if clean.Len() > 0 {
			break // 遇到空格或其他字符停止
		}
	}
	s = clean.String()

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
