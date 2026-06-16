package youtube

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

// FetchYoutubePosts scrapes Shorts post metrics directly from the current view by matching exact class elements.
func FetchYoutubePosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("YT_FETCH", "开始 YouTube Studio 正式版极速采集流程")

	// 1. 创建局部静音上下文
	silentCtx, silentCancel := chromedp.NewContext(ctx,
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	defer silentCancel()

	// 2. 强行导航至后台主页
	targetURL := "https://studio.youtube.com/"
	logger.Print("YT_FETCH", "正在直达 YouTube Studio 后台: "+targetURL)
	if err := chromedp.Run(silentCtx, chromedp.Navigate(targetURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate to studio failed: %w", err)
	}

	// 3. 执行内核重定向直达 Shorts 页面
	var currentURL string
	if err := chromedp.Run(silentCtx, chromedp.Location(&currentURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("get current location failed: %w", err)
	}

	shortsURL := strings.TrimSuffix(currentURL, "/") + "/videos/short"
	logger.Print("YT_FETCH", "重定向切换至 Shorts 后台目标页 -> "+shortsURL)

	redirectJS := fmt.Sprintf(`window.location.href = "%s";`, shortsURL)
	if err := chromedp.Run(silentCtx, chromedp.Evaluate(redirectJS, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("location redirect failed: %w", err)
	}

	// 4. 等待表格内容渲染
	logger.Print("YT_FETCH", "等待数据表格完全渲染...")
	waitListSel := `ytcp-video-section-content#video-list`
	if err := chromedp.Run(silentCtx, chromedp.WaitVisible(waitListSel, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("wait for video list table failed: %w", err)
	}
	time.Sleep(3 * time.Second) // 预留稳定性数据行落字缓冲

	// 5. 注入高精度 Class 定位采集脚本
	logger.Print("YT_FETCH", "注入精准节点嗅探与去重脚本...")
	runScript := `
    (() => {
        window._ytPostsData = [];
        let postsMap = new Map();

        function formatYoutubeDate(rawDateStr) {
            if (!rawDateStr) return "";
            let cleanStr = rawDateStr.replace(/[\u2000-\u206F\u2070-\u209F\u20A0-\u20CF\u20D0-\u20FF\u2100-\u214F]/g, " ");
            cleanStr = cleanStr.replace(/\s+/g, " ").trim();
            try {
                let parsedTimestamp = Date.parse(cleanStr);
                if (isNaN(parsedTimestamp)) {
                    // 适配可能存在的中文排版格式
                    let match = cleanStr.match(/(\d{4})[-年](\d{1,2})[-月](\d{1,2})/);
                    if (match) {
                        let pad = (n) => n.length < 2 ? '0' + n : n;
                        return match[1] + '-' + pad(match[2]) + '-' + pad(match[3]) + ' 00:00:00';
                    }
                    return "";
                }
                let d = new Date(parsedTimestamp);
                let pad = (n) => n < 10 ? '0' + n : n;
                return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' 00:00:00';
            } catch (e) { return ""; }
        }

        function shatterAndFindAll(selector) {
            let results = [];
            function findElements(node) {
                if (!node) return;
                if (node.nodeType === Node.ELEMENT_NODE) {
                    if (node.matches(selector)) { results.push(node); }
                    if (node.shadowRoot) findElements(node.shadowRoot);
                }
                if (node.tagName === 'IFRAME') {
                    try {
                        let doc = node.contentDocument || node.contentWindow.document;
                        if (doc) findElements(doc.body);
                    } catch(e) {}
                }
                let child = node.firstChild;
                while (child) {
                    findElements(child);
                    child = child.nextSibling;
                }
            }
            findElements(document.body);
            return results;
        }

        function queryInsideShadow(rootNode, subSelector) {
            if (!rootNode) return null;
            let target = null;
            function traverse(node) {
                if (!node || target) return;
                if (node.nodeType === Node.ELEMENT_NODE) {
                    if (node.matches(subSelector)) { target = node; return; }
                    if (node.shadowRoot) traverse(node.shadowRoot);
                }
                let child = node.firstChild;
                while (child) { traverse(child); child = child.nextSibling; }
            }
            traverse(rootNode);
            return target;
        }

        let cells = shatterAndFindAll('ytcp-video-row');
        cells.forEach(row => {
            let titleNode = queryInsideShadow(row, 'a#video-title');
            if (titleNode) {
                let editHref = titleNode.getAttribute('href') || ""; 
                let titleText = titleNode.innerText || "";
                
                let videoId = editHref.replace("/video/", "").replace("/edit", "").trim();
                
                if (videoId && !postsMap.has(videoId)) {
                    
                    // 1. 🌟 核心修复：精准隔离文本节点，剔除下方的 status div 影响
                    let dateCell = queryInsideShadow(row, '.tablecell-date');
                    let rawDate = "";
                    if (dateCell) {
                        let textParts = [];
                        dateCell.childNodes.forEach(n => {
                            if (n.nodeType === Node.TEXT_NODE) textParts.push(n.textContent);
                        });
                        rawDate = textParts.join(" ").trim();
                        if (!rawDate) {
                            rawDate = (dateCell.innerText || "").replace(/Published|Scheduled|Private|Unlisted/gi, "").trim();
                        }
                    }
                    let standardDate = formatYoutubeDate(rawDate);

                    // 2. 抓取观看量
                    let viewsCell = queryInsideShadow(row, '.tablecell-views');
                    let viewsStr = viewsCell ? viewsCell.innerText.replace(/[\r\n]+/g, "").trim() : "0";

                    // 3. 抓取评论数
                    let commentsCell = queryInsideShadow(row, '.tablecell-comments a');
                    let commentsStr = commentsCell ? commentsCell.innerText.trim() : "0";

                    // 4. 抓取点赞数（后台结构中若未提供 likes 独立展示，默认同步为 0 或解析相关单元格）
                    let likesCell = queryInsideShadow(row, '.tablecell-likes .likes-label') || queryInsideShadow(row, '[class*="likes"]');
                    let likesStr = likesCell ? likesCell.innerText.trim() : "0";

                    postsMap.set(videoId, {
                        title: titleText,
                        video_id: videoId,
                        publishTime: standardDate,
                        views: viewsStr,
                        likes: likesStr,
                        comments: commentsStr,
                        shares: "0"
                    });
                }
            }
        });

        window._ytPostsData = Array.from(postsMap.values());
        window._ytScrollDone = true;
    })()
	`

	if err := chromedp.Run(silentCtx, chromedp.Evaluate(runScript, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("inject scroll script failed: %w", err)
	}

	// 6. 捞回干净的数据数组
	var jsResult []map[string]string
	if err := chromedp.Run(silentCtx, chromedp.Evaluate("window._ytPostsData || []", &jsResult)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("retrieve standard json data failed: %w", err)
	}

	if len(jsResult) == 0 {
		return scraper.FetchResult{}, fmt.Errorf("数据列解析完成，但未成功生成有效映射记录")
	}

	// 7. 格式化数据并逐条规范打印
	var posts []scraper.Post
	logger.Print("YT_FETCH", "开始解析并逐条打印 YouTube Shorts 发文明细:")

	for idx, raw := range jsResult {
		fullShortsURL := "https://www.youtube.com/shorts/" + raw["video_id"]

		logger.Print("YT_DATA", fmt.Sprintf(
			"发文 [%d] -> 时间: %s | 观看: %s | 评论: %s | 点赞: %s | 链接: %s",
			idx+1,
			raw["publishTime"],
			raw["views"],
			raw["comments"],
			raw["likes"],
			fullShortsURL,
		))

		posts = append(posts, scraper.Post{
			Title:       raw["title"],
			Link:        fullShortsURL,
			PublishTime: raw["publishTime"],
			Likes:       parseYoutubeMetric(raw["likes"]),
			Comments:    parseYoutubeMetric(raw["comments"]),
			Shares:      0,
			Views:       parseYoutubeMetric(raw["views"]),
		})
	}

	logger.Print("YT_FETCH", fmt.Sprintf("YouTube 抓取执行完毕。本次成功收录 %d 条有效 Shorts 视频数据", len(posts)))
	return scraper.FetchResult{Posts: posts}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func parseYoutubeMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "–" {
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
