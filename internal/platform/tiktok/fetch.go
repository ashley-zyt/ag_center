package tiktok

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

// FetchTikTokPosts scrapes post metrics from TikTok Studio by penetrating nested iframes.
func FetchTikTokPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("TT_FETCH", "开始 TikTok Studio 跨框架穿透抓取流程")

	silentCtx, silentCancel := chromedp.NewContext(ctx,
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	defer silentCancel()

	// 1. 强行导航至后台管理页
	targetURL := "https://www.tiktok.com/tiktokstudio/content"
	logger.Print("TT_FETCH", "正在导航至创作后台: "+targetURL)
	if err := chromedp.Run(silentCtx, chromedp.Navigate(targetURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate failed: %w", err)
	}

	// 2. 预留页面加载硬缓冲
	time.Sleep(5 * time.Second)

	// 3. 注入具备浏览器端【时间清洗能力】与【iframe 穿透能力】的 JS 脚本
	runScrollScript := `
	(() => {
		window._ttPostsData = [];
		let postsMap = new Map();
		let currentRound = 0;
		let maxRounds = 3;
		let totalCheckCount = 0;

		// 格式化 TikTok 的 "Jun 3, 2:41 AM" 为 Rails 喜欢的 "YYYY-MM-DD HH:mm:ss"
		function formatTikTokDate(rawDateStr) {
			if (!rawDateStr) return "";
			// 强行清洗掉 TikTok 塞进去的各种恶心的不可见特殊窄空格 (\u202f 等)
			let cleanStr = rawDateStr.replace(/[\u2000-\u206F\u2070-\u209F\u20A0-\u20CF\u20D0-\u20FF\u2100-\u214F]/g, " ").trim();
			
			// 补齐年份
			let currentYear = new Date().getFullYear();
			let finalStr = currentYear + " " + cleanStr;
			
			try {
				let parsedTimestamp = Date.parse(finalStr);
				if (isNaN(parsedTimestamp)) return "";
				
				// 转换为标准 ISO/Rails 时间格式
				let d = new Date(parsedTimestamp);
				let pad = (n) => n < 10 ? '0' + n : n;
				return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + 
				       pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
			} catch (e) {
				return "";
			}
		}

		function findElementsInAllContexts(selector) {
			let elements = Array.from(document.querySelectorAll(selector));
			function penetrate(node) {
				if (!node) return;
				if (node.tagName === 'IFRAME') {
					try {
						let doc = node.contentDocument || node.contentWindow.document;
						if (doc) {
							elements = elements.concat(Array.from(doc.querySelectorAll(selector)));
							penetrate(doc.body);
						}
					} catch(e) {}
				}
				if (node.children) {
					for (let child of node.children) { penetrate(child); }
				}
			}
			penetrate(document.body);
			return elements;
		}

		let penetrationTimer = setInterval(() => {
			totalCheckCount++;
			let cells = findElementsInAllContexts('div[data-tt="components_PostTable_Absolute"]');
			
			if (cells.length > 0) {
				cells.forEach(cell => {
					let infoContainer = cell.querySelector('div[data-tt="components_PostInfoCell_Container"]');
					let linkNode = infoContainer ? infoContainer.querySelector('a') : null;
					
					if (linkNode) {
						let href = linkNode.getAttribute('href') || "";
						if (href && !postsMap.has(href)) {
							let titleText = linkNode.innerText || "";
							let timeNode = cell.querySelector('div[data-tt="components_PublishStageLabel_FlexCenter"]');
							
							// 🌟 在网页内部直接洗干净时间文本
							let rawTime = timeNode ? timeNode.innerText : "";
							let standardTime = formatTikTokDate(rawTime);

							let statContainer = cell.querySelector('div[data-tt="components_RowLayout_FlexRow_5"]');
							let viewsStr = "0", likesStr = "0", commentsStr = "0";
							if (statContainer && statContainer.children.length >= 3) {
								viewsStr = statContainer.children[0].innerText || "0";
								likesStr = statContainer.children[1].innerText || "0";
								commentsStr = statContainer.children[2].innerText || "0";
							}

							postsMap.set(href, {
								title: titleText,
								link: href,
								publishTime: standardTime, // 此时已经是 "2026-06-03 02:41:00"
								views: viewsStr,
								likes: likesStr,
								comments: commentsStr,
								shares: "0"
							});
						}
					}
				});

				currentRound++;
				if (currentRound >= maxRounds) {
					clearInterval(penetrationTimer);
					window._ttPostsData = Array.from(postsMap.values());
					window._ttScrollDone = true;
					return;
				}
				window.scrollBy(0, 1200);
			} else {
				if (totalCheckCount > 30) { 
					clearInterval(penetrationTimer);
					window._ttPostsData = [];
					window._ttScrollDone = true;
				}
			}
		}, 500);
	})()
	`

	if err := chromedp.Run(silentCtx, chromedp.Evaluate(runScrollScript, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("inject script failed: %w", err)
	}

	// 4. 等待浏览器信号
	var isDone bool
	for i := 0; i < 20; i++ {
		_ = chromedp.Run(silentCtx, chromedp.Evaluate("window._ttScrollDone || false", &isDone))
		if isDone {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// 5. 捞回干净数据
	var jsResult []map[string]string
	if err := chromedp.Run(silentCtx, chromedp.Evaluate("window._ttPostsData || []", &jsResult)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("retrieve json failed: %w", err)
	}

	if len(jsResult) == 0 {
		return scraper.FetchResult{}, fmt.Errorf("未能在页面上捕获到任何发文数据")
	}

	// 6. 拼装转换并打印
	var posts []scraper.Post
	logger.Print("TT_FETCH", "开始解析并逐条打印 TikTok 发文明细:")

	for idx, raw := range jsResult {
		fullLink := raw["link"]
		if !strings.HasPrefix(fullLink, "http") {
			fullLink = "https://www.tiktok.com" + "/" + strings.TrimPrefix(fullLink, "/")
		}

		// 精准、规范的单条发文打印
		logger.Print("TT_DATA", fmt.Sprintf(
			"发文 [%d] -> 时间: %s | 链接: %s | 标题: %s",
			idx+1,
			raw["publishTime"], // 这里打印出来的已经是清洗过的标准时间字符串
			fullLink,
			truncate(raw["title"], 30),
		))

		posts = append(posts, scraper.Post{
			Title:       raw["title"],
			Link:        fullLink,
			PublishTime: raw["publishTime"], // 干净格式传递给上层调度区
			Likes:       parseTikTokMetric(raw["likes"]),
			Comments:    parseTikTokMetric(raw["comments"]),
			Shares:      0,
			Views:       parseTikTokMetric(raw["views"]),
		})
	}

	logger.Print("TT_FETCH", fmt.Sprintf("TikTok 抓取执行完毕。本次成功收录 %d 条发文", len(posts)))
	return scraper.FetchResult{Posts: posts}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func parseTikTokMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}
	s = strings.ReplaceAll(s, ",", "")
	var clean strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || r == 'k' || r == 'm' || r == 'b' {
			clean.WriteRune(r)
		} else {
			break
		}
	}
	s = clean.String()
	multiplier := 1.0
	if strings.HasSuffix(s, "k") {
		multiplier = 1000.0
		s = strings.TrimSuffix(s, "k")
	}
	if strings.HasSuffix(s, "m") {
		multiplier = 1000000.0
		s = strings.TrimSuffix(s, "m")
	}
	if strings.HasSuffix(s, "b") {
		multiplier = 1000000000.0
		s = strings.TrimSuffix(s, "b")
	}
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(val * multiplier)
}
