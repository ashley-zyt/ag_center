package instagram

import (
	"context"
	"strconv" // 引入字符串转数字的工具包
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"

	"github.com/chromedp/chromedp"
)

func FetchInstagramPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("INS_FETCH", "开始执行 Instagram 详情页深度采集与数据清洗...")

	silentCtx, silentCancel := chromedp.NewContext(ctx, chromedp.WithErrorf(func(string, ...interface{}) {}))
	defer silentCancel()

	// 1. 导航到主页
	if err := chromedp.Run(silentCtx, chromedp.Navigate(req.SourceURL)); err != nil {
		return scraper.FetchResult{}, err
	}
	time.Sleep(6 * time.Second)

	// 2. 扫描发文链接
	var links []string
	_ = chromedp.Run(silentCtx, chromedp.Evaluate(`
		Array.from(document.querySelectorAll('a[href*="/p/"], a[href*="/reel/"]'))
		.map(a => a.href)
		.slice(0, 5)
	`, &links))

	var posts []scraper.Post
	for _, link := range links {
		var data map[string]string

		// 3. 进入详情页抓取原始字符串数据
		err := chromedp.Run(silentCtx,
			chromedp.Navigate(link),
			chromedp.Sleep(5*time.Second),
			chromedp.Evaluate(`(() => {
				let textEl = document.querySelector('span.x126k92a');
				let title = textEl ? textEl.innerText.trim() : "无标题";

				let targetSection = document.querySelector('main[role="main"] section.x1o61qjw');
				
				let likes = "0";
				let comments = "0";
				let shares = "0";

				if (targetSection) {
					let rowDiv = targetSection.querySelector('div.x6s0dn4.x78zum5');
					if (rowDiv) {
						let children = Array.from(rowDiv.children);
						for (let i = 0; i < children.length; i++) {
							let child = children[i];
							let html = child.innerHTML;

							if (html.includes('aria-label="Like"')) {
								let next = children[i + 1];
								if (next && next.tagName === 'SPAN' && /^\d+$/.test(next.innerText.trim())) {
									likes = next.innerText.trim();
								}
							}
							if (html.includes('aria-label="Comment"')) {
								let next = children[i + 1];
								if (next && next.tagName === 'SPAN' && /^\d+$/.test(next.innerText.trim())) {
									comments = next.innerText.trim();
								}
							}
							if (html.includes('aria-label="Share"')) {
								let next = children[i + 1];
								if (next && next.tagName === 'SPAN' && /^\d+$/.test(next.innerText.trim())) {
									shares = next.innerText.trim();
								}
							}
						}
					}
				}

				return {
					"title": title,
					"time": document.querySelector('time')?.getAttribute('datetime') || "Unknown",
					"likes": likes,
					"comments": comments,
					"shares": shares,
					"link": window.location.href
				};
			})()`, &data),
		)

		if err != nil {
			logger.Print("INS_WARN", "详情页解析失败: "+link)
			continue
		}

		// 🌟 4. 数据类型清洗：将抓取到的 String 转换为 Int，确保更新 API 能够正确识别
		likesCount, _ := strconv.Atoi(data["likes"])
		commentsCount, _ := strconv.Atoi(data["comments"])
		sharesCount, _ := strconv.Atoi(data["shares"])

		// 5. 组装最终传输对象
		postItem := scraper.Post{
			Title:       data["title"],
			Link:        data["link"],
			PublishTime: data["time"],
			Likes:       likesCount,    // 写入转换后的 int，不再传字符串
			Comments:    commentsCount, // 写入转换后的 int，不再传字符串
			Shares:      sharesCount,   // 写入转换后的 int，不再传字符串
		}

		// 打印清洗后的数据，用于最终校验
		// fmt.Printf("\n>>> [数据清洗完毕] 链接: %s | 点赞: %d | 评论: %d | 分享: %d\n",
		// 	postItem.Link, postItem.Likes, postItem.Comments, postItem.Shares)

		posts = append(posts, postItem)
		time.Sleep(2 * time.Second)
	}

	return scraper.FetchResult{Posts: posts}, nil
}
