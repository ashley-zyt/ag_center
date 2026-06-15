package facebook

import (
	"context"
	"fmt"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"

	"github.com/chromedp/chromedp"
)

func FetchFacebookPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("FB_FETCH", "启用防御性抓取模式: "+req.SourceURL)

	silentCtx, silentCancel := chromedp.NewContext(ctx, chromedp.WithErrorf(func(string, ...interface{}) {}))
	defer silentCancel()

	if err := chromedp.Run(silentCtx, chromedp.Navigate(req.SourceURL)); err != nil {
		return scraper.FetchResult{}, err
	}
	time.Sleep(5 * time.Second)

	// 滚动触发懒加载
	_ = chromedp.Run(silentCtx, chromedp.Evaluate(`window.scrollBy(0, 2000);`, nil))
	time.Sleep(3 * time.Second)

	var results []map[string]string
	err := chromedp.Run(silentCtx, chromedp.Evaluate(`
		(() => {
			let list = [];
			let cards = document.querySelectorAll('div[aria-posinset]');
			
			cards.forEach((card, idx) => {
				// 1. 标题抓取
				let titleEl = card.querySelector('div[data-ad-rendering-role="story_message"]');
				let titleText = titleEl ? titleEl.innerText.trim() : "";
				if (!titleText || titleText.length < 5) return;

				// 2. 时间戳抓取：优先 aria-label，无效则回退至 innerText
				let timeEl = card.querySelector('a[tabindex="0"]');
				let timeText = timeEl ? (timeEl.getAttribute('aria-label') || timeEl.innerText.trim()) : "Today";

				// 3. 深度 URL 嗅探
				let url = "";
				let links = card.querySelectorAll('a');
				for(let a of links) {
					if (a.href && (a.href.includes('/posts/') || a.href.includes('/reel/'))) {
						url = a.href;
						break;
					}
				}
				// 暴力扫描属性
				if (!url) {
					let all = card.querySelectorAll('*');
					for(let el of all) {
						for(let attr of el.attributes) {
							if (attr.value.includes('facebook.com') && (attr.value.includes('/posts/') || attr.value.includes('/reel/'))) {
								url = attr.value;
								break;
							}
						}
						if (url) break;
					}
				}

				list.push({
					"index": (idx + 1).toString(),
					"title": titleText,
					"time": timeText,
					"link": url
				});
			});
			return list;
		})()
	`, &results))

	if err != nil {
		return scraper.FetchResult{}, err
	}

	// 组装结果
	var posts []scraper.Post
	for _, item := range results {
		fmt.Printf("[序列: %s] [时间: %s] [链接: %s] 标题: %s\n", item["index"], item["time"], item["link"], item["title"])

		posts = append(posts, scraper.Post{
			Title:       item["title"],
			Link:        item["link"],
			PublishTime: item["time"],
		})
	}

	return scraper.FetchResult{Posts: posts}, nil
}
