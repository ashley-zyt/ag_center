package twitter

import (
	"context"
	"encoding/json"
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

	// ctx已由调用方配置错误抑制，直接使用(避免NewContext创建多余空白标签页)

	// 1. 直接导航到目标用户主页(而非固定/home再点profile，避免多账号场景进错页面)
	targetURL := strings.TrimSpace(req.SourceURL)
	if targetURL == "" {
		targetURL = "https://x.com/home"
	}
	if err := chromedp.Run(ctx, chromedp.Navigate(targetURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate to profile failed: %w", err)
	}

	// 2. 等待页面主体加载(profile头部或推文列表出现)
	_ = chromedp.Run(ctx,
		chromedp.PollFunction(`() => {
			// 等待profile头部(含用户信息) 或 推文列表出现
			const header = document.querySelector('div[data-testid="UserName"], h2[role="heading"]');
			const cells = document.querySelectorAll('div[data-testid="cellInnerDiv"]');
			return !!(header || cells.length > 0);
		}`, nil, chromedp.WithPollingTimeout(15*time.Second), chromedp.WithPollingInterval(500*time.Millisecond)),
	)
	time.Sleep(2 * time.Second)

	// 3. 等待内容容器初始加载
	cellSel := `div[data-testid="cellInnerDiv"]`
	if err := chromedp.Run(ctx, chromedp.WaitVisible(cellSel, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("wait for posts failed: %w", err)
	}

	// 3.5 提取总粉丝数: 使用PollFunction等待元素渲染完成，并增加多种fallback策略
	var totalFollowers int
	var totalPosts int
	var followersText string

	// 先尝试从页面嵌入的JSON数据中提取(最可靠,不依赖DOM渲染时序)
	extractEmbeddedJS := `(() => {
		try {
			// 尝试从script标签中的__INITIAL_STATE__或类似变量提取
			const scripts = document.querySelectorAll('script');
			for (const s of scripts) {
				const text = s.textContent || '';
				// 查找followers_count字段
				const m = text.match(/"followers_count"\s*:\s*(\d+)/);
				const sM = text.match(/"statuses_count"\s*:\s*(\d+)/);
				if (m) {
					return JSON.stringify({
						followers: m[1],
						posts: sM ? sM[1] : "0"
					});
				}
			}
			// 尝试从window对象中提取
			if (window.__INITIAL_STATE__) {
				const str = JSON.stringify(window.__INITIAL_STATE__);
				const m = str.match(/"followers_count"\s*:\s*(\d+)/);
				const sM = str.match(/"statuses_count"\s*:\s*(\d+)/);
				if (m) {
					return JSON.stringify({
						followers: m[1],
						posts: sM ? sM[1] : "0"
					});
				}
			}
		} catch(e) {}
		return "";
	})()`

	var embeddedData string
	if err := chromedp.Run(ctx, chromedp.Evaluate(extractEmbeddedJS, &embeddedData)); err == nil && embeddedData != "" {
		// 解析嵌入数据
		type embeddedResult struct {
			Followers string `json:"followers"`
			Posts     string `json:"posts"`
		}
		var emb embeddedResult
		if jsonErr := parseJSON(embeddedData, &emb); jsonErr == nil {
			if emb.Followers != "" {
				if v, parseErr := strconv.Atoi(emb.Followers); parseErr == nil {
					totalFollowers = v
					followersText = emb.Followers
				}
			}
			if emb.Posts != "" {
				if v, parseErr := strconv.Atoi(emb.Posts); parseErr == nil {
					totalPosts = v
				}
			}
		}
	}

	if totalFollowers > 0 {
		logger.Print("TW_FETCH", fmt.Sprintf("账号总粉丝数(从嵌入JSON提取): %d", totalFollowers))
	} else {
		// Fallback: 从DOM提取, 使用PollFunction等待元素出现(最长15秒)
		followersJS := `(() => {
			// 查找所有href包含/followers的a标签(覆盖/verified_followers和/followers两种URL格式)
			const links = document.querySelectorAll('a[href*="/followers"]');
			// 多语言粉丝关键词(包括单数Follower和复数Followers)
			const followerRe = /follower|粉丝|フォロワー|팔로워|подписчик|abonn/i;
			for (const a of links) {
				const href = a.getAttribute('href') || '';
				const text = (a.textContent || '').trim().toLowerCase();
				// 排除/followers_you_follow等非粉丝数链接
				if (/\/followers_you_follow|\/followers_known|\/followers\/suggest|\/followers\?/.test(href)) continue;
				// 验证文本包含粉丝关键词(处理Follower/Followers单复数)
				if (!followerRe.test(text)) continue;
				// 查找第一个含数字的span(粉丝数在Followers文本span之前)
				const spans = a.querySelectorAll('span');
				for (const s of spans) {
					const t = (s.textContent || '').trim();
					// 匹配纯数字或带K/M/万后缀的数字
					if (/^[\d,\.\s\u00a0]+[kKmM万]?$/.test(t) && /\d/.test(t) && t.length <= 20) {
						return t;
					}
				}
				// Fallback: 从a标签文本中正则提取数字
				const m = (a.textContent || '').match(/([\d,\.]+\s*[kKmM万]?)/);
				if (m && /\d/.test(m[1])) return m[1];
			}
			// 更宽松的fallback: 直接在profile区域找所有a标签,匹配包含Follower文字的
			const profileHeader = document.querySelector('header[role="banner"], div[data-testid="primaryColumn"]') || document;
			const allLinks = profileHeader.querySelectorAll('a');
			for (const a of allLinks) {
				const href = a.getAttribute('href') || '';
				const text = (a.textContent || '').trim();
				if (/follower/i.test(text) && /\d/.test(text)) {
					const m = text.match(/([\d,\.]+\s*[kKmM万]?)/);
					if (m) return m[1];
				}
			}
			return "";
		})()`

		// 使用PollFunction等待粉丝数元素出现
		for attempt := 0; attempt < 10; attempt++ {
			if err := chromedp.Run(ctx, chromedp.Evaluate(followersJS, &followersText)); err == nil && followersText != "" {
				totalFollowers = parseTwitterMetric(followersText)
				if totalFollowers > 0 {
					break
				}
			}
			time.Sleep(1 * time.Second)
		}

		if totalFollowers > 0 {
			logger.Print("TW_FETCH", fmt.Sprintf("账号总粉丝数(从DOM提取): %d (原始: %s)", totalFollowers, followersText))
		} else {
			logger.Print("TW_FETCH", "TW_FETCH_SUB 未找到粉丝数(DOM和嵌入JSON均未提取到)")
		}
	}

	// 同样尝试从DOM提取帖子数(totalPosts),如果嵌入JSON未提供
	if totalPosts == 0 {
		postsCountJS := `(() => {
			// 查找导航栏中Posts/Replies等tab的数字, 或查找profile头部的posts数
			// 方式1: 查找包含数字+Posts/帖子关键词的元素
			const allLinks = document.querySelectorAll('a');
			for (const a of allLinks) {
				const href = a.getAttribute('href') || '';
				const text = (a.textContent || '').trim();
				// 匹配类似 "123 Posts" 的格式(在profile导航tab中)
				if (/posts|帖子|ポスト|게시물/i.test(text) && /\d/.test(text)) {
					// 排除非profile的链接
					if (!href.includes('/status/') && !href.includes('/followers')) {
						const m = text.match(/([\d,\.]+\s*[kKmM万]?)/);
						if (m) return m[1];
					}
				}
			}
			return "";
		})()`
		var postsText string
		_ = chromedp.Run(ctx, chromedp.Evaluate(postsCountJS, &postsText))
		if postsText != "" {
			totalPosts = parseTwitterMetric(postsText)
		}
	}

	// 立即上报账号统计数据(粉丝数+发帖数),失败不中断流程
	scraper.ReportAccountStats(ctx, logger, req.AccountID, totalFollowers, totalPosts, req.AccountStatsEndpoint)

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

	if err := chromedp.Run(ctx, chromedp.Evaluate(runScrollScript, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("inject scroll script failed: %w", err)
	}

	// 5. 等待 JS 结束标记并取回数据
	var isDone bool
	for i := 0; i < 10; i++ {
		_ = chromedp.Run(ctx, chromedp.Evaluate("window._xScrollDone || false", &isDone))
		if isDone {
			break
		}
		time.Sleep(1 * time.Second)
	}

	var jsResult []map[string]string
	if err := chromedp.Run(ctx, chromedp.Evaluate("window._xPostsData || []", &jsResult)); err != nil {
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
	result := scraper.FetchResult{
		Posts:          posts,
		TotalFollowers: totalFollowers,
		TotalPosts:     totalPosts,
	}
	return scraper.SanitizeResult(result), nil
}

// parseJSON 解析JSON字符串到目标结构
func parseJSON(data string, v interface{}) error {
	return json.Unmarshal([]byte(data), v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// parseTwitterMetric 解析Twitter数字，支持:
// "8", "1,234", "12.5K", "1.5M", "1.2万", "1 234"(空格千分), "1,2K"(欧洲小数)
func parseTwitterMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	// 移除空格和不间断空格
	s = strings.ReplaceAll(s, "\u00a0", "")
	s = strings.ReplaceAll(s, " ", "")

	multiplier := 1.0
	hasSuffix := true
	switch {
	case strings.HasSuffix(s, "万"):
		multiplier = 10000.0
		s = strings.TrimSuffix(s, "万")
	case strings.HasSuffix(s, "亿"):
		multiplier = 100000000.0
		s = strings.TrimSuffix(s, "亿")
	case strings.HasSuffix(s, "億"):
		multiplier = 100000000.0
		s = strings.TrimSuffix(s, "億")
	default:
		if len(s) > 0 {
			last := s[len(s)-1:]
			switch last {
			case "k":
				multiplier = 1000.0
				s = s[:len(s)-1]
			case "m":
				multiplier = 1000000.0
				s = s[:len(s)-1]
			case "b":
				multiplier = 1000000000.0
				s = s[:len(s)-1]
			default:
				hasSuffix = false
			}
		}
	}

	// 处理千分位/小数分隔符
	if strings.Contains(s, ",") && strings.Contains(s, ".") {
		if strings.LastIndex(s, ",") > strings.LastIndex(s, ".") {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.ReplaceAll(s, ",", ".")
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	} else if strings.Contains(s, ",") {
		if hasSuffix {
			s = strings.ReplaceAll(s, ",", ".")
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(val * multiplier)
}
