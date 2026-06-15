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

// FetchPosts scrapes posts from X / Twitter using robust in-browser JS evaluation.
func FetchPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("TW_FETCH", "开始抓取 X 流程: "+req.SourceURL)

	// 🌟 创建一个新的局部上下文，强行注入 WithErrorf 静音器
	// 这会直接吞掉所有底层类似 "could not unmarshal event" 的非业务干扰错误
	silentCtx, silentCancel := chromedp.NewContext(ctx,
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	defer silentCancel()

	// 1. 打开首页（使用 silentCtx）
	if err := chromedp.Run(silentCtx, chromedp.Navigate("https://x.com/home")); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate home failed: %w", err)
	}

	// 2. 点击个人主页
	profileBtnSel := `a[data-testid="AppTabBar_Profile_Link"]`
	if err := chromedp.Run(silentCtx,
		chromedp.WaitVisible(profileBtnSel, chromedp.ByQuery),
		chromedp.Click(profileBtnSel, chromedp.ByQuery),
	); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("click profile link failed: %w", err)
	}

	// 3. 等待内容容器初始加载
	cellSel := `div[data-testid="cellInnerDiv"]`
	if err := chromedp.Run(silentCtx, chromedp.WaitVisible(cellSel, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("wait for posts failed: %w", err)
	}
	time.Sleep(2 * time.Second)

	// 4. 执行滚动采集
	logger.Print("TW_FETCH", "正在执行动态滚动采集...")
	runScrollScript := `
	(() => {
		window._xPostsData = [];
		let postsMap = new Map();
		let currentRound = 0;
		let maxRounds = 3;

		// 🌟 【新增验证第一步】：根据 DOM 结构，动态提取当前主页的正确 Handle (例如: @DanielaHig92951)
		let targetHandle = "";
		let pageUserContainer = document.querySelector('div[data-testid="UserName"]');
		if (pageUserContainer) {
			// 从包含 @ 字符的文本节点中精准匹配出用户的 Handle
			let handleMatch = pageUserContainer.innerText.match(/@\w+/);
			if (handleMatch) {
				targetHandle = handleMatch[0].toLowerCase().trim();
			}
		}

		// 兜底策略：如果 DOM 尚未完全加载，则从当前浏览器路径名中提取
		if (!targetHandle) {
			let pathParts = window.location.pathname.split('/');
			if (pathParts[1]) {
				targetHandle = "@" + pathParts[1].toLowerCase().trim();
			}
		}

		console.log("【安全防污染验证】当前主页合法发文者 Handle 被锁定为: " + targetHandle);

		function collectData() {
			let cells = document.querySelectorAll('div[data-testid="cellInnerDiv"]');
			cells.forEach(cell => {
				let textNode = cell.querySelector('div[data-testid="tweetText"]');
				let linkNode = cell.querySelector('div[data-testid="User-Name"] a[href*="/status/"]');
				
				if (textNode && linkNode) {
					// 🌟 【新增验证第二步】：提取当前这条推文真实的发布者 Handle
					let tweetUserContainer = cell.querySelector('div[data-testid="User-Name"]');
					if (tweetUserContainer) {
						let tweetUserText = tweetUserContainer.innerText;
						let tweetHandleMatch = tweetUserText.match(/@\w+/);
						
						if (tweetHandleMatch) {
							let currentTweetHandle = tweetHandleMatch[0].toLowerCase().trim();
							
							// 🌟 【新增验证第三步】：进行匹配，若不成功，则判定为嵌入的热点/广告，直接略过
							if (targetHandle && currentTweetHandle !== targetHandle) {
								// console.log("过滤掉非本主页发文: " + currentTweetHandle);
								return; 
							}
						}
					}

					let link = linkNode.getAttribute('href');
					if (!postsMap.has(link)) {
						let timeNode = cell.querySelector('div[data-testid="User-Name"] time');
						let replyNode = cell.querySelector('button[data-testid="reply"]');
						let retweetNode = cell.querySelector('button[data-testid="retweet"]') || cell.querySelector('button[data-testid="unretweet"]');
						let likeNode = cell.querySelector('button[data-testid="like"]') || cell.querySelector('button[data-testid="unlike"]');
						let viewNode = cell.querySelector('a[href*="/analytics"]');

						postsMap.set(link, {
							title: textNode.innerText || "",
							link: link,
							publishTime: timeNode ? (timeNode.getAttribute('datetime') || "") : "",
							comments: replyNode ? (replyNode.innerText || replyNode.getAttribute('aria-label') || "") : "",
							shares: retweetNode ? (retweetNode.innerText || retweetNode.getAttribute('aria-label') || "") : "",
							likes: likeNode ? (likeNode.innerText || likeNode.getAttribute('aria-label') || "") : "",
							views: viewNode ? (viewNode.innerText || viewNode.getAttribute('aria-label') || "") : ""
						});
					}
				}
			});
		}

		let timer = setInterval(() => {
			collectData();
			currentRound++;
			if (currentRound >= maxRounds) {
				clearInterval(timer);
				window._xPostsData = Array.from(postsMap.values());
				window._xScrollDone = true;
				return;
			}
			window.scrollBy(0, 1400);
		}, 2000);
	})()
	`

	if err := chromedp.Run(silentCtx, chromedp.Evaluate(runScrollScript, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("inject scroll script failed: %w", err)
	}

	// 5. 等待 JS 结束标记并取回数据
	var isDone bool
	for i := 0; i < 10; i++ {
		_ = chromedp.Run(silentCtx, chromedp.Evaluate("window._xScrollDone || false", &isDone))
		if isDone {
			break
		}
		time.Sleep(1 * time.Second)
	}

	var jsResult []map[string]string
	if err := chromedp.Run(silentCtx, chromedp.Evaluate("window._xPostsData || []", &jsResult)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("retrieve standard json data failed: %w", err)
	}

	// 6. 数据序列化拼装并进行精准打印
	var posts []scraper.Post
	logger.Print("TW_FETCH", "开始解析并打印抓取到的发文明细:")

	for idx, raw := range jsResult {
		// 补全完整 URL
		fullLink := raw["link"]
		if !strings.HasPrefix(fullLink, "http") {
			fullLink = "https://x.com" + fullLink
		}

		// 循环打印每一条推文的完整详细数据
		logger.Print("TW_DATA", fmt.Sprintf(
			"发文 [%d] -> 时间: %s | 链接: %s | 标题: %s",
			idx+1,
			raw["publishTime"],
			fullLink,
			truncate(raw["title"], 30), // 标题较长时截断 30 字符展示，防止日志刷屏
		))

		posts = append(posts, scraper.Post{
			Title:       raw["title"],
			Link:        fullLink,
			PublishTime: raw["publishTime"],
			Likes:       parseTwitterMetric(raw["likes"]),
			Comments:    parseTwitterMetric(raw["comments"]),
			Shares:      parseTwitterMetric(raw["shares"]),
			Views:       parseTwitterMetric(raw["views"]),
		})
	}

	logger.Print("TW_FETCH", fmt.Sprintf("抓取流执行完毕。本次成功收录 %d 条有效发文", len(posts)))
	return scraper.FetchResult{Posts: posts}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func parseTwitterMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || strings.HasPrefix(s, "0 ") {
		return 0
	}
	s = strings.ReplaceAll(s, ",", "")

	var clean strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || r == 'k' || r == 'm' || r == 'b' {
			clean.WriteRune(r)
		} else if clean.Len() > 0 {
			break
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
