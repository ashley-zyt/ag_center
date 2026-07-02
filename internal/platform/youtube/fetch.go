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

// FetchYoutubePosts scrapes Shorts post metrics directly from the current view and visits detail pages for likes.
func FetchYoutubePosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	// [精炼] 合并初始化日志
	logger.Print("YT_FETCH", "初始化采集: 导航至 Shorts 后台并准备注入脚本...")

	// 1. 创建局部静音上下文
	silentCtx, silentCancel := chromedp.NewContext(ctx,
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	defer silentCancel()

	// 2. 强行导航至后台主页
	targetURL := "https://studio.youtube.com/"
	if err := chromedp.Run(silentCtx, chromedp.Navigate(targetURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate to studio failed: %w", err)
	}

	// 3. 执行内核重定向直达 Shorts 页面
	var currentURL string
	if err := chromedp.Run(silentCtx, chromedp.Location(&currentURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("get current location failed: %w", err)
	}

	shortsURL := strings.TrimSuffix(currentURL, "/") + "/videos/short"
	redirectJS := fmt.Sprintf(`window.location.href = "%s";`, shortsURL)
	if err := chromedp.Run(silentCtx, chromedp.Evaluate(redirectJS, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("location redirect failed: %w", err)
	}

	// 4. 等待表格内容渲染
	waitListSel := `ytcp-video-section-content#video-list`
	if err := chromedp.Run(silentCtx, chromedp.WaitVisible(waitListSel, chromedp.ByQuery)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("wait for video list table failed: %w", err)
	}
	time.Sleep(3 * time.Second) // 预留稳定性数据行落字缓冲

	// 5. 注入高精度 Class 定位采集脚本
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

                    let viewsCell = queryInsideShadow(row, '.tablecell-views');
                    let viewsStr = viewsCell ? viewsCell.innerText.replace(/[\r\n]+/g, "").trim() : "0";

                    let commentsCell = queryInsideShadow(row, '.tablecell-comments a');
                    let commentsStr = commentsCell ? commentsCell.innerText.trim() : "0";

                    postsMap.set(videoId, {
                        title: titleText,
                        video_id: videoId,
                        publishTime: standardDate,
                        views: viewsStr,
                        comments: commentsStr,
                        shares: "0"
                    });
                }
            }
        });

        window._ytPostsData = Array.from(postsMap.values());
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

	// 7. 遍历数据：打开新标签页进入 Shorts 详情页单独抓取点赞量，随后关闭
	var posts []scraper.Post

	// [精炼] 提示解析出的数量
	logger.Print("YT_FETCH", fmt.Sprintf("嗅探到 %d 条记录，开始追溯点赞明细...", len(jsResult)))

	for idx, raw := range jsResult {
		fullShortsURL := "https://www.youtube.com/shorts/" + raw["video_id"]
		likesStr := "0"

		detailCtx, detailCancel := chromedp.NewContext(silentCtx)

		likeSelectors := []string{
			`div.ytSpecButtonShapeWithLabelLabel span`,
			`yt-spec-button-shape-next span.yt-spec-button-shape-next__text`,
			`button[aria-label*="like"] span`,
			`.like-button-view-model span`,
		}

		timeoutCtx, timeoutCancel := context.WithTimeout(detailCtx, 15*time.Second)

		err := chromedp.Run(timeoutCtx,
			chromedp.Navigate(fullShortsURL),
			chromedp.Sleep(2*time.Second),
			chromedp.WaitVisible(`ytd-shorts-video-renderer`, chromedp.ByQuery),
		)

		if err == nil {
			for _, selector := range likeSelectors {
				err = chromedp.Run(timeoutCtx, chromedp.WaitVisible(selector, chromedp.ByQuery))
				if err == nil {
					err = chromedp.Run(timeoutCtx, chromedp.Text(selector, &likesStr, chromedp.ByQuery))
					if err == nil {
						break
					}
				}
			}
		}

		if err != nil {
			timeoutCancel()
			retryCtx, retryCancel := context.WithTimeout(detailCtx, 10*time.Second)
			time.Sleep(3 * time.Second)

			retryErr := chromedp.Run(retryCtx,
				chromedp.WaitVisible(`ytd-shorts-video-renderer`, chromedp.ByQuery),
			)

			if retryErr == nil {
				for _, selector := range likeSelectors {
					retryErr = chromedp.Run(retryCtx, chromedp.WaitVisible(selector, chromedp.ByQuery))
					if retryErr == nil {
						retryErr = chromedp.Run(retryCtx, chromedp.Text(selector, &likesStr, chromedp.ByQuery))
						if retryErr == nil {
							err = nil
							break
						}
					}
				}
			}

			retryCancel()
		}

		timeoutCancel()
		detailCancel()

		if err != nil {
			logger.Print("YT_WARN", fmt.Sprintf("视频 [%s] 点赞抓取失败(超时/受限)", raw["video_id"]))
			likesStr = "0"
		}

		// 处理点赞数为 "like" 或空的情况
		likesStr = strings.TrimSpace(likesStr)
		if likesStr == "" || strings.EqualFold(likesStr, "like") {
			likesStr = "0"
		}

		// [精炼] 截取日期的前半部分 (去除 00:00:00)
		shortDate := raw["publishTime"]
		if parts := strings.Split(shortDate, " "); len(parts) > 0 {
			shortDate = parts[0]
		}

		// [精炼] 数据行排版紧凑对齐
		logger.Print("YT_DATA", fmt.Sprintf(
			"#%02d | 日期: %s | 观看: %-3s | 评论: %-2s | 点赞: %-3s | %s",
			idx+1, shortDate, raw["views"], raw["comments"], likesStr, fullShortsURL,
		))

		posts = append(posts, scraper.Post{
			Title:       raw["title"],
			Link:        fullShortsURL,
			PublishTime: raw["publishTime"],
			Likes:       parseYoutubeMetric(likesStr),
			Comments:    parseYoutubeMetric(raw["comments"]),
			Shares:      0,
			Views:       parseYoutubeMetric(raw["views"]),
		})
	}

	// [精炼] 结束语精简
	logger.Print("YT_FETCH", fmt.Sprintf("采集完成: 共收录 %d 条有效数据", len(posts)))
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
