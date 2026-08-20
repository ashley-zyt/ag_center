package youtube

import (
	"context"
	"fmt"
	"regexp"
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

	// ctx已由调用方配置错误抑制，直接使用(避免NewContext创建多余空白标签页)

	// 2. 强行导航至后台主页
	targetURL := "https://studio.youtube.com/"
	if err := chromedp.Run(ctx, chromedp.Navigate(targetURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("navigate to studio failed: %w", err)
	}

	// 2.5 提取粉丝数：先获取当前Studio URL中的Channel ID，跳转到公开频道页提取订阅数，再返回Studio
	var studioURL string
	_ = chromedp.Run(ctx, chromedp.Location(&studioURL))
	var totalFollowers int
	var totalPosts int
	if studioURL != "" {
		// 从Studio URL中提取channel ID: https://studio.youtube.com/channel/UCxxx/...
		re := regexp.MustCompile(`/channel/([^/]+)`)
		if matches := re.FindStringSubmatch(studioURL); len(matches) >= 2 {
			channelID := matches[1]
			publicChannelURL := "https://www.youtube.com/channel/" + channelID
			logger.Print("YT_FETCH", fmt.Sprintf("正在跳转至公开频道页提取粉丝数: %s", publicChannelURL))
			if navErr := chromedp.Run(ctx, chromedp.Navigate(publicChannelURL)); navErr == nil {
				time.Sleep(5 * time.Second)
				// 以 yt-page-header-renderer 为唯一根容器(页面唯一), 提取订阅数和视频数
				// 覆盖所有已知结构类型:
				// Type1(中文自己频道): @handle • 36 部影片 (无订阅数)
				// Type2(英文新频道): @handle • 1 subscriber • 17 videos
				// Type3(英文空频道): @handle • 21 videos (无订阅数)
				// Type4(新包装): <yt-page-header-renderer> 包裹 <yt-page-header-view-model>
				var result struct {
					SubText string `json:"sub_text"`
					VidText string `json:"vid_text"`
				}
				extractJS := `(() => {
					// 以唯一根容器为起点
					const root = document.querySelector('yt-page-header-renderer.pageHeaderRendererHost')
						|| document.querySelector('yt-page-header-view-model.ytPageHeaderViewModelHost');
					if (!root) return {sub_text: '', vid_text: ''};

					const meta = root.querySelector('yt-content-metadata-view-model');
					if (!meta) return {sub_text: '', vid_text: ''};

					// 收集所有文本span
					const spans = meta.querySelectorAll('span.ytContentMetadataViewModelMetadataText');
					let subText = '';
					let vidText = '';

					// 多语言关键词: 订阅者 / 视频
					const subRe = /subscriber|订阅|suscriptor|abonn|iscrit|登録者|구독자|подписчик|inscrits/i;
					const vidRe = /\bvideo(s)?\b|部影片|支影片|個影片|个影片|動画|本の動画|비디오|동영상|видео|vidéo|vídeo|film/i;

					for (const span of spans) {
						// 优先从 aria-label 获取(更精确)
						const label = span.getAttribute('aria-label');
						const text = (span.textContent || '').trim();
						const source = label || text;
						if (!source) continue;

						if (!subText && /\d/.test(source) && subRe.test(source)) {
							subText = source;
						}
						if (!vidText && /\d/.test(source) && vidRe.test(source)) {
							vidText = source;
						}
					}

					// 若未通过关键词匹配到订阅数, 尝试从含aria-label的span获取(该aria-label通常包含完整订阅描述)
					if (!subText) {
						for (const span of spans) {
							const label = span.getAttribute('aria-label');
							if (label && /\d/.test(label) && !label.startsWith('@')) {
								// 排除视频数的aria-label(通常包含video关键词)
								if (!vidRe.test(label)) {
									subText = label;
									break;
								}
							}
						}
					}

					// 若仍未找到视频数, 遍历span找含数字且包含video关键词的
					if (!vidText) {
						for (const span of spans) {
							const text = (span.textContent || '').trim();
							if (text && /\d/.test(text) && vidRe.test(text)) {
								vidText = text;
								break;
							}
						}
					}

					return {sub_text: subText, vid_text: vidText};
				})()`
				_ = chromedp.Run(ctx, chromedp.Evaluate(extractJS, &result))

				// 解析订阅数
				if result.SubText != "" {
					numRe := regexp.MustCompile(`(\d[\d,\. \x{00a0}]*\s*[kKmMbB万亿億]?)`)
					if numMatches := numRe.FindStringSubmatch(strings.TrimSpace(result.SubText)); len(numMatches) >= 2 {
						totalFollowers = parseYouTubeSubCount(numMatches[1])
						logger.Print("YT_FETCH", fmt.Sprintf("账号总粉丝数(订阅者): %d (原始: %s)", totalFollowers, result.SubText))
					} else {
						logger.Print("YT_FETCH", fmt.Sprintf("YT_FETCH_SUB 解析订阅者数量失败，未找到数字，原始文本: %s", result.SubText))
					}
				} else {
					logger.Print("YT_FETCH", "YT_FETCH_SUB 频道页面未找到订阅者数量(该频道可能为自己的频道页面或订阅数为0不显示)")
				}

				// 解析视频数
				totalPosts = 0
				if result.VidText != "" {
					vidNumRe := regexp.MustCompile(`(\d[\d,\. \x{00a0}]*)`)
					if vidMatches := vidNumRe.FindStringSubmatch(strings.TrimSpace(result.VidText)); len(vidMatches) >= 2 {
						clean := strings.ReplaceAll(vidMatches[1], ",", "")
						clean = strings.ReplaceAll(clean, "\u00a0", "")
						clean = strings.ReplaceAll(clean, " ", "")
						if v, err := strconv.Atoi(clean); err == nil {
							totalPosts = v
							logger.Print("YT_FETCH", fmt.Sprintf("频道视频数: %d (原始: %s)", totalPosts, result.VidText))
						}
					}
				}

				// 立即上报账号统计数据(粉丝数+发帖数),失败不中断流程
				scraper.ReportAccountStats(ctx, logger, req.AccountID, totalFollowers, totalPosts, req.AccountStatsEndpoint)

				// 返回Studio页面继续原有流程
				logger.Print("YT_FETCH", "粉丝数/视频数提取完成，返回Studio页面...")
				_ = chromedp.Run(ctx, chromedp.Navigate(studioURL))
				time.Sleep(3 * time.Second)
			}
		}
	}

	// 3. 获取当前Studio URL用于构建Tab地址
	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("get current location failed: %w", err)
	}
	studioBase := strings.TrimSuffix(currentURL, "/")

	// 4. 采集Shorts (/videos/short) 和 长视频 (/videos)
	var posts []scraper.Post

	// Shorts采集
	shortsTabURL := studioBase + "/videos/short"
	shortsPosts, shortsErr := collectStudioTab(ctx, logger, shortsTabURL, true)
	if shortsErr != nil {
		logger.Print("YT_WARN", fmt.Sprintf("Shorts采集遇到问题: %v", shortsErr))
	}
	posts = append(posts, shortsPosts...)
	logger.Print("YT_FETCH", fmt.Sprintf("Shorts采集完成: %d 条", len(shortsPosts)))

	// 长视频采集
	videosTabURL := studioBase + "/videos"
	videosPosts, videosErr := collectStudioTab(ctx, logger, videosTabURL, false)
	if videosErr != nil {
		logger.Print("YT_WARN", fmt.Sprintf("长视频采集遇到问题: %v", videosErr))
	}
	posts = append(posts, videosPosts...)
	logger.Print("YT_FETCH", fmt.Sprintf("长视频采集完成: %d 条", len(videosPosts)))

	// [精炼] 结束语精简
	logger.Print("YT_FETCH", fmt.Sprintf("采集完成(Shorts+长视频): 共收录 %d 条有效数据", len(posts)))
	result := scraper.FetchResult{
		Posts:          posts,
		TotalFollowers: totalFollowers,
		TotalPosts:     totalPosts,
	}
	return scraper.SanitizeResult(result), nil
}

// collectStudioTab 采集Studio指定Tab(Shorts或Videos)的视频列表和点赞数
// isShorts=true时访问 /videos/short 详情页用 /shorts/{id}; false时访问 /videos 详情页用 /watch?v={id}
func collectStudioTab(ctx context.Context, logger *logx.Logger, tabURL string, isShorts bool) ([]scraper.Post, error) {
	tag := "YT_SHORTS"
	detailURLPrefix := "https://www.youtube.com/shorts/"
	if !isShorts {
		tag = "YT_VIDEOS"
		detailURLPrefix = "https://www.youtube.com/watch?v="
	}

	// 导航到Tab页面
	logger.Print(tag, fmt.Sprintf("正在导航至: %s", tabURL))
	redirectJS := fmt.Sprintf(`window.location.href = "%s";`, tabURL)
	if err := chromedp.Run(ctx, chromedp.Evaluate(redirectJS, nil)); err != nil {
		return nil, fmt.Errorf("redirect to tab failed: %w", err)
	}

	// 等待URL切换到目标路径(Shorts含/videos/short, 长视频含/videos且不含/short)
	var urlCheckJS string
	if isShorts {
		urlCheckJS = `() => window.location.href.includes('/videos/short')`
	} else {
		urlCheckJS = `() => window.location.href.includes('/videos') && !window.location.href.includes('/videos/short')`
	}
	_ = chromedp.Run(ctx,
		chromedp.PollFunction(urlCheckJS, nil,
			chromedp.WithPollingTimeout(15*time.Second), chromedp.WithPollingInterval(500*time.Millisecond)),
	)

	// YouTube Studio是SPA: 使用PollFunction(而非无超时的WaitVisible)等待列表容器或空状态
	// 空频道时#video-list可能不存在(被空状态组件替代), 需同时检测两种情况, 避免长时间挂起
	waitListPoll := `() => {
		const list = document.querySelector('ytcp-video-section-content#video-list');
		if (list) return true;
		// 检测空状态: 页面已渲染但无视频(空状态容器/提示文字)
		const emptyState = document.querySelector('ytcp-empty-state-view-model, [class*="empty"], #empty-state');
		if (emptyState) return true;
		// 检测Studio内容区已渲染(body有内容但无视频列表)
		const content = document.querySelector('#content, #page-manager');
		if (content && content.offsetHeight > 100) return true;
		return false;
	}`
	_ = chromedp.Run(ctx,
		chromedp.PollFunction(waitListPoll, nil,
			chromedp.WithPollingTimeout(15*time.Second), chromedp.WithPollingInterval(500*time.Millisecond)),
	)

	// 等待ytcp-video-row行元素出现 或 检测到空状态
	// 使用较短超时: 10秒内无视频行则判定为空频道
	_ = chromedp.Run(ctx,
		chromedp.PollFunction(`() => {
			const rows = document.querySelectorAll('ytcp-video-row');
			if (rows.length > 0) return true;
			// 检测空状态文字(多语言: 没有视频/No videos/動画がありません 等)
			const bodyText = document.body.innerText || '';
			if (/(no videos|no content|没有视频|沒有影片|暂无视频|暫無影片|動画がありません|비디오가 없습니다|нет видео|aucune vidéo|no hay vídeos)/i.test(bodyText)) return true;
			return false;
		}`, nil,
			chromedp.WithPollingTimeout(10*time.Second), chromedp.WithPollingInterval(500*time.Millisecond)),
	)
	time.Sleep(2 * time.Second)

	// 滚动表格区域以触发懒加载, 确保前10行被渲染(不需要滚动到底加载全部30条)
	scrollJS := `
		(() => {
			const table = document.querySelector('ytcp-video-section-content#video-list');
			if (!table) return;
			const scrollContainer = table.closest('ytcp-scroll-container, [class*="scroll"], #content, #page-manager');
			let sc = scrollContainer;
			// 向上滚先
			if (sc) sc.scrollTop = 0;
			window.scrollTo(0, 0);
			// 只滚动一小段距离触发前10条的懒加载(每行约80px, 10条约800px, 留余量滚1200px)
			window.scrollTo(0, 600);
		})()
	`
	_ = chromedp.Run(ctx, chromedp.Evaluate(scrollJS, nil))
	time.Sleep(1500 * time.Millisecond)

	// 注入采集脚本(重复注入以确保拿到滚动后的数据)
	if err := chromedp.Run(ctx, chromedp.Evaluate(ytStudioCollectJS, nil)); err != nil {
		return nil, fmt.Errorf("inject collect script failed: %w", err)
	}

	// 捞回数据
	var jsResult []map[string]string
	if err := chromedp.Run(ctx, chromedp.Evaluate("window._ytPostsData || []", &jsResult)); err != nil {
		return nil, fmt.Errorf("retrieve data failed: %w", err)
	}
	if len(jsResult) == 0 {
		logger.Print(tag, "该Tab下未发现视频数据")
		return nil, nil
	}

	// 每个Tab最多只处理前10条
	const maxPostsPerTab = 10
	if len(jsResult) > maxPostsPerTab {
		logger.Print(tag, fmt.Sprintf("嗅探到 %d 条记录，仅处理前 %d 条", len(jsResult), maxPostsPerTab))
		jsResult = jsResult[:maxPostsPerTab]
	} else {
		logger.Print(tag, fmt.Sprintf("嗅探到 %d 条记录，开始追溯点赞明细...", len(jsResult)))
	}

	// 遍历详情页获取点赞数(直接在当前标签页导航, 不创建新标签页, 避免标签页管理混乱)
	var posts []scraper.Post
	for idx, raw := range jsResult {
		videoID := strings.TrimSpace(raw["video_id"])
		// 长视频: 链接获取不到不算有效发文; Shorts保持原有行为(videoID为空时JS层已过滤)
		if videoID == "" {
			logger.Print(tag, fmt.Sprintf("#%02d 跳过: 无法获取视频链接", idx+1))
			continue
		}

		fullDetailURL := detailURLPrefix + videoID
		likesStr := fetchVideoLikes(ctx, logger, tag, fullDetailURL, videoID, isShorts)

		shortDate := raw["publishTime"]
		if parts := strings.Split(shortDate, " "); len(parts) > 0 {
			shortDate = parts[0]
		}

		typeLabel := "Shorts"
		if !isShorts {
			typeLabel = "Video"
		}
		logger.Print(tag, fmt.Sprintf(
			"#%02d [%s] 日期: %s | 观看: %-3s | 评论: %-2s | 点赞: %-3s | %s",
			idx+1, typeLabel, shortDate, raw["views"], raw["comments"], likesStr, fullDetailURL,
		))

		posts = append(posts, scraper.Post{
			Title:       raw["title"],
			Link:        fullDetailURL,
			PublishTime: raw["publishTime"],
			Likes:       parseYoutubeMetric(likesStr),
			Comments:    parseYoutubeMetric(raw["comments"]),
			Shares:      0,
			Views:       parseYoutubeMetric(raw["views"]),
		})
	}

	// 所有详情页采集完毕后, 导航回Studio Tab页面, 确保下一个Tab采集从Studio开始
	backCtx, backCancel := context.WithTimeout(ctx, 15*time.Second)
	_ = chromedp.Run(backCtx, chromedp.Navigate(tabURL))
	// 使用PollFunction替代WaitVisible, 空频道页#video-list可能不存在
	_ = chromedp.Run(backCtx,
		chromedp.PollFunction(`() => {
			const list = document.querySelector('ytcp-video-section-content#video-list');
			if (list) return true;
			const content = document.querySelector('#content, #page-manager');
			if (content && content.offsetHeight > 100) return true;
			return false;
		}`, nil, chromedp.WithPollingTimeout(10*time.Second), chromedp.WithPollingInterval(500*time.Millisecond)),
	)
	backCancel()
	time.Sleep(2 * time.Second)

	return posts, nil
}

// fetchVideoLikes 导航到视频详情页并提取点赞数。
// 解决 Shorts 自动连播问题: 视频播放完后 YouTube 会自动跳转到别的大V视频, 导致点赞数采错。
// 防护策略: 提取前后校验当前 URL 是否仍指向目标 videoID, 若已跳转则重新导航重试。
func fetchVideoLikes(ctx context.Context, logger *logx.Logger, tag, fullDetailURL, videoID string, isShorts bool) string {
	pageWaitSelector := `ytd-reel-video-renderer`
	if !isShorts {
		pageWaitSelector = `ytd-watch-flexy`
	}

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		detailCtx, cancel := context.WithTimeout(ctx, 20*time.Second)

		err := chromedp.Run(detailCtx,
			chromedp.Navigate(fullDetailURL),
			chromedp.Sleep(2*time.Second),
			chromedp.WaitVisible(pageWaitSelector, chromedp.ByQuery),
		)
		if err != nil {
			cancel()
			continue
		}

		// 等待点赞按钮就绪
		_ = chromedp.Run(detailCtx,
			chromedp.PollFunction(`() => {
				const actionBar = document.querySelector('reel-action-bar-view-model, .ytwReelActionBarViewModelHost, #top-level-buttons-computed, #menu-container ytd-menu-renderer');
				const searchRoot = actionBar || document;
				const btn = searchRoot.querySelector('like-button-view-model, .ytLikeButtonViewModelHost, #segmented-like-button');
				if (!btn) return false;
				function checkNode(root) {
					if (!root) return false;
					const spans = root.querySelectorAll('div.ytSpecButtonShapeWithLabelLabel span, span.ytAttributedStringHost, span.yt-core-attributed-string, .yt-spec-button-shape-next__text, .yt-spec-button-shape-next__button-text-content');
					for (const s of spans) {
						const t = (s.textContent || '').trim();
						if (t && t.length <= 15 && /\d/.test(t) && !/^(Like|赞)$/.test(t)) return true;
					}
					const btns = root.querySelectorAll('button[aria-label*="like"], button[aria-label*="Like"]');
					for (const b of btns) { if (/\d/.test(b.getAttribute('aria-label')||'')) return true; }
					const all = root.querySelectorAll('*');
					for (const el of all) { if (el.shadowRoot && checkNode(el.shadowRoot)) return true; }
					return false;
				}
				if (checkNode(btn)) return true;
				if (btn.shadowRoot && checkNode(btn.shadowRoot)) return true;
				return false;
			}`, nil, chromedp.WithPollingTimeout(10*time.Second), chromedp.WithPollingInterval(300*time.Millisecond)),
		)

		// 提取前校验 URL: 确保仍停留在目标视频, 未被自动连播跳转
		var currentURL string
		_ = chromedp.Run(detailCtx, chromedp.Location(&currentURL))
		if !isTargetVideoURL(currentURL, videoID) {
			logger.Print(tag+"_WARN", fmt.Sprintf("视频 [%s] 提取前发现页面已跳转(%s), 第%d次重试", videoID, currentURL, attempt))
			cancel()
			continue
		}

		// 提取点赞数
		var likesStr string
		err = chromedp.Run(detailCtx, chromedp.Evaluate(ytExtractLikesJS, &likesStr))

		// 提取后再校验一次 URL, 防止提取过程中发生跳转
		_ = chromedp.Run(detailCtx, chromedp.Location(&currentURL))
		if err == nil && isTargetVideoURL(currentURL, videoID) {
			cancel()
			likesStr = strings.TrimSpace(likesStr)
			if likesStr == "" || strings.EqualFold(likesStr, "like") {
				likesStr = "0"
			}
			return likesStr
		}

		logger.Print(tag+"_WARN", fmt.Sprintf("视频 [%s] 提取后页面已跳转或提取失败, 第%d次重试", videoID, attempt))
		cancel()
	}

	logger.Print(tag+"_WARN", fmt.Sprintf("视频 [%s] 点赞抓取失败(自动连播跳转或超时)", videoID))
	return "0"
}

// isTargetVideoURL 判断当前 URL 是否仍指向目标 videoID。
// YouTube 详情页存在多种链接格式, 已知:
//   - https://www.youtube.com/watch?v=VIDEO_ID
//   - https://www.youtube.com/shorts/VIDEO_ID
// 校验 videoID 后必须紧跟分隔符(参数/路径/锚点/结尾), 避免子串误判。
func isTargetVideoURL(currentURL, videoID string) bool {
	if videoID == "" || currentURL == "" {
		return false
	}

	// watch?v=VIDEO_ID 格式: videoID 后应为 &、# 或字符串结尾
	if strings.Contains(currentURL, "v="+videoID) {
		idx := strings.Index(currentURL, "v="+videoID) + len("v=") + len(videoID)
		if idx >= len(currentURL) || strings.ContainsRune("&#", rune(currentURL[idx])) {
			return true
		}
	}

	// shorts/VIDEO_ID 格式: videoID 后应为 ?、/、# 或字符串结尾
	if strings.Contains(currentURL, "/shorts/"+videoID) {
		idx := strings.Index(currentURL, "/shorts/"+videoID) + len("/shorts/") + len(videoID)
		if idx >= len(currentURL) || strings.ContainsRune("?/#", rune(currentURL[idx])) {
			return true
		}
	}

	return false
}

// ytStudioCollectJS 是在Studio页面采集视频列表的JS脚本(Shorts和Videos通用)
const ytStudioCollectJS = `
(() => {
    window._ytPostsData = [];
    let postsMap = new Map();

    function formatYoutubeDate(rawDateStr) {
        if (!rawDateStr) return "";
        let cleanStr = rawDateStr.replace(/[\u2000-\u206F\u2070-\u209F\u20A0-\u20CF\u20D0-\u20FF\u2100-\u214F]/g, " ");
        cleanStr = cleanStr.replace(/\s+/g, " ").trim();

        const monthMap = {
            'jan':1,'january':1,'feb':2,'february':2,'mar':3,'march':3,'apr':4,'april':4,
            'may':5,'jun':6,'june':6,'jul':7,'july':7,'aug':8,'august':8,'sep':9,'sept':9,'september':9,
            'oct':10,'october':10,'nov':11,'november':11,'dec':12,'december':12,
            'oca':1,'ocak':1,'şub':2,'sub':2,'şubat':2,'subat':2,'mar':3,'mart':3,'nis':4,'nisan':4,
            'may':5,'mayıs':5,'mayis':5,'haz':6,'haziran':6,'tem':7,'temmuz':7,'ağu':8,'agu':8,'ağustos':8,'agustos':8,
            'eyl':9,'eylül':9,'eylul':9,'eki':10,'ekim':10,'kas':11,'kasım':11,'kasim':11,'ara':12,'aralık':12,'aralik':12,
            'ene':1,'enero':1,'feb':2,'febrero':2,'mar':3,'marzo':3,'abr':4,'abril':4,'may':5,'mayo':5,
            'jun':6,'junio':6,'jul':7,'julio':7,'ago':8,'agosto':8,'sep':9,'sept':9,'septiembre':9,
            'oct':10,'octubre':10,'nov':11,'noviembre':11,'dic':12,'diciembre':12,
            'janv':1,'janvier':1,'févr':2,'fevr':2,'février':2,'fevrier':2,'mars':3,'avr':4,'avril':4,
            'mai':5,'juin':6,'juil':7,'juillet':7,'août':8,'aout':8,'sept':9,'septembre':9,
            'oct':10,'octobre':10,'nov':11,'novembre':11,'déc':12,'dec':12,'décembre':12,'decembre':12,
            'jan':1,'januar':1,'feb':2,'februar':2,'mär':3,'mar':3,'märz':3,'marz':3,'apr':4,'april':4,
            'mai':5,'jun':6,'juni':6,'jul':7,'juli':7,'aug':8,'august':8,'sep':9,'sept':9,'september':9,
            'okt':10,'oktober':10,'nov':11,'november':11,'dez':12,'dezember':12,
            'jan':1,'janeiro':1,'fev':2,'fevereiro':2,'mar':3,'março':3,'marco':3,'abr':4,'abril':4,
            'mai':5,'maio':5,'jun':6,'junho':6,'jul':7,'julho':7,'ago':8,'agosto':8,
            'set':9,'setembro':9,'out':10,'outubro':10,'nov':11,'novembro':11,'dez':12,'dezembro':12,
            'gen':1,'gennaio':1,'feb':2,'febbraio':2,'mar':3,'marzo':3,'apr':4,'aprile':4,'mag':5,'maggio':5,
            'giu':6,'giugno':6,'lug':7,'luglio':7,'ago':8,'agosto':8,'set':9,'settembre':9,
            'ott':10,'ottobre':10,'nov':11,'novembre':11,'dic':12,'dicembre':12,
            'jan':1,'januari':1,'feb':2,'februari':2,'mrt':3,'maart':3,'apr':4,'april':4,'mei':5,
            'jun':6,'juni':6,'jul':7,'juli':7,'aug':8,'augustus':8,'sep':9,'september':9,
            'okt':10,'oktober':10,'nov':11,'november':11,'dec':12,'december':12,
            'sty':1,'styczeń':1,'styczen':1,'lut':2,'luty':2,'mar':3,'marzec':3,'kwi':4,'kwiecień':4,'kwiecien':4,
            'maj':5,'cze':6,'czerwiec':6,'lip':7,'lipiec':7,'sie':8,'sierpień':8,'sierpien':8,
            'wrz':9,'wrzesień':9,'wrzesien':9,'paź':10,'paz':10,'październik':10,'pazdziernik':10,
            'lis':11,'listopad':11,'gru':12,'grudzień':12,'grudzien':12,
            'янв':1,'январь':1,'января':1,'фев':2,'февраль':2,'февраля':2,'мар':3,'март':3,'марта':3,
            'апр':4,'апрель':4,'апреля':4,'май':5,'мая':5,'июн':6,'июнь':6,'июня':6,'июл':7,'июль':7,'июля':7,
            'авг':8,'август':8,'августа':8,'сен':9,'сент':9,'сентябрь':9,'сентября':9,
            'окт':10,'октябрь':10,'октября':10,'ноя':11,'ноябрь':11,'ноября':11,'дек':12,'декабрь':12,'декабря':12,
            '1月':1,'2月':2,'3月':3,'4月':4,'5月':5,'6月':6,'7月':7,'8月':8,'9月':9,'10月':10,'11月':11,'12月':12,
            '1월':1,'2월':2,'3월':3,'4월':4,'5월':5,'6월':6,'7월':7,'8월':8,'9월':9,'10월':10,'11월':11,'12월':12,
            'jan':1,'januari':1,'feb':2,'februari':2,'mar':3,'maret':3,'apr':4,'mei':5,'jun':6,'juni':6,
            'jul':7,'juli':7,'agt':8,'agu':8,'agustus':8,'sep':9,'sept':9,'september':9,
            'okt':10,'oktober':10,'nov':11,'november':11,'des':12,'desember':12,
            'يناير':1,'فبراير':2,'مارس':3,'أبريل':4,'مايو':5,'يونيو':6,'يوليو':7,'أغسطس':8,'سبتمبر':9,'أكتوبر':10,'نوفمبر':11,'ديسمبر':12,
            'jan':1,'januari':1,'feb':2,'februari':2,'mar':3,'mars':3,'apr':4,'april':4,'maj':5,
            'jun':6,'juni':6,'jul':7,'juli':7,'aug':8,'augusti':8,'sep':9,'sept':9,'september':9,
            'okt':10,'oktober':10,'nov':11,'november':11,'dec':12,'des':12,'december':12,
            'tam':1,'tamm':1,'tammikuu':1,'hel':2,'helm':2,'helmikuu':2,'maa':3,'maalis':3,'maaliskuu':3,
            'huh':4,'huhti':4,'huhtikuu':4,'tou':5,'touko':5,'toukokuu':5,'kes':6,'kesä':6,'kesa':6,'kesäkuu':6,'kesakuu':6,
            'hei':7,'heinä':7,'heina':7,'heinäkuu':7,'heinakuu':7,'elo':8,'elokuu':8,
            'syy':9,'syys':9,'syyskuu':9,'lok':10,'loka':10,'lokakuu':10,'marras':11,'marraskuu':11,'jou':12,'joulu':12,'joulukuu':12,
            'led':1,'leden':1,'úno':2,'uno':2,'únor':2,'unor':2,'bře':3,'bre':3,'březen':3,'brezen':3,
            'dub':4,'duben':4,'kvě':5,'kve':5,'květen':5,'kveten':5,'čvn':6,'cvn':6,'červen':6,'cerven':6,
            'čvc':7,'cvc':7,'červenec':7,'cervenec':7,'srp':8,'srpen':8,'zář':9,'zar':9,'září':9,'zari':9,
            'říj':10,'rij':10,'říjen':10,'rijen':10,'lis':11,'listopad':11,'pro':12,'prosinec':12,
            'jan':1,'január':1,'febr':2,'február':2,'márc':3,'marc':3,'március':3,'marcius':3,
            'ápr':4,'apr':4,'április':4,'aprilis':4,'máj':5,'maj':5,'május':5,'majus':5,
            'jún':6,'jun':6,'június':6,'junius':6,'júl':7,'jul':7,'július':7,'julius':7,
            'aug':8,'augusztus':8,'szept':9,'szep':9,'szeptember':9,
            'okt':10,'október':10,'oktober':10,'nov':11,'november':11,'dec':12,'december':12,
            'ian':1,'ianuarie':1,'febr':2,'februarie':2,'mart':3,'martie':3,'apr':4,'aprilie':4,
            'mai':5,'iun':6,'iunie':6,'iul':7,'iulie':7,'aug':8,'august':8,'sept':9,'septembrie':9,
            'oct':10,'octombrie':10,'nov':11,'noiembrie':11,'dec':12,'decembrie':12,
            'ม.ค.':1,'ก.พ.':2,'มี.ค.':3,'เม.ย.':4,'พ.ค.':5,'มิ.ย.':6,'ก.ค.':7,'ส.ค.':8,'ก.ย.':9,'ต.ค.':10,'พ.ย.':11,'ธ.ค.':12,
            'जन':1,'जनवरी':1,'फ़र':2,'फ़रवरी':2,'फर':2,'फरवरी':2,'मार्च':3,'अप्रैल':4,'अप्रेल':4,'मई':5,
            'जून':6,'जुला':7,'जुलाई':7,'अग':8,'अगस्त':8,'सित':9,'सितंबर':9,'सितम्बर':9,
            'अक्टू':10,'अक्टूबर':10,'नव':11,'नवंबर':11,'नवम्बर':11,'दिस':12,'दिसंबर':12,'दिसम्बर':12,
        };

        try {
            let parsedTimestamp = Date.parse(cleanStr);
            if (!isNaN(parsedTimestamp)) {
                let d = new Date(parsedTimestamp);
                let pad = (n) => n < 10 ? '0' + n : n;
                return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' 00:00:00';
            }
        } catch (e) {}

        let cnMatch = cleanStr.match(/(\d{4})[-年](\d{1,2})[-月](\d{1,2})/);
        if (cnMatch) {
            let pad = (n) => n.length < 2 ? '0' + n : n;
            return cnMatch[1] + '-' + pad(cnMatch[2]) + '-' + pad(cnMatch[3]) + ' 00:00:00';
        }

        let normalized = cleanStr.toLowerCase();
        normalized = normalized.normalize ? normalized.normalize('NFD').replace(/[\u0300-\u036f]/g, '') : normalized;

        let foundMonth = 0, matchedMonthLen = 0;
        let monthKeys = Object.keys(monthMap).sort((a, b) => b.length - a.length);
        for (const key of monthKeys) {
            let keyNorm = key.normalize ? key.normalize('NFD').replace(/[\u0300-\u036f]/g, '') : key;
            let re = new RegExp('(^|[^a-zäöüışğç0-9])' + keyNorm.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '($|[^a-zäöüışğç0-9])', 'i');
            if (re.test(normalized) && key.length > matchedMonthLen) {
                foundMonth = monthMap[key];
                matchedMonthLen = key.length;
            }
        }

        if (foundMonth > 0) {
            let nums = cleanStr.match(/\d{1,4}/g);
            if (nums) {
                let day = 1, year = new Date().getFullYear();
                let candidates = nums.map(n => parseInt(n, 10));
                for (const n of candidates) {
                    if (n >= 1970 && n <= 2100) { year = n; }
                    else if (n >= 1 && n <= 31 && day === 1) { day = n; }
                }
                let pad = (n) => n < 10 ? '0' + n : String(n);
                return year + '-' + pad(foundMonth) + '-' + pad(day) + ' 00:00:00';
            }
        }
        return "";
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

// ytExtractLikesJS 提取YouTube视频点赞数(同时支持Shorts和长视频页面)
// 核心策略: 只在like-button-view-model宿主元素及其shadowRoot内查找, 绝不全局遍历document.body(避免匹配到推荐视频的观看数等)
const ytExtractLikesJS = `
(() => {
    // 从文本中提取数字部分(处理"10"、"1.2K"、"123万"、"1,234"等格式)
    function extractNum(text) {
        if (!text) return "";
        let t = text.trim().replace(/\s+/g, " ");
        // 必须包含数字
        if (!/\d/.test(t)) return "";
        // 文本不能太长(点赞数都是短文本)
        if (t.length > 20) return "";
        // 排除明显非点赞数的文本
        if (/view|subscriber|comment|share|ago|year|month|day|week|hour|minute|second|thousand|million|billion/i.test(t) && !/^[\d.,]+\s*[KMBkmb万]?$/.test(t)) return "";
        let m = t.match(/[\d]+[,\d]*\.?\d*[KkMmBb万]?/);
        return m ? m[0] : "";
    }

    // 从aria-label中提取数字(如 "like this video along with 10 other people")
    function extractFromAriaLabel(label) {
        if (!label) return "";
        // 必须包含like关键词和数字
        if (!/like|Like|点赞/i.test(label)) return "";
        if (!/\d/.test(label)) return "";
        let m = label.match(/([\d][\d,.]*\s*[KkMmBb万]?)/);
        if (!m) return "";
        // 过滤纯年份等(4位数但不是点赞数的情况)
        let num = m[1].replace(/,/g, "").replace(/\s/g, "");
        if (/^\d{4}$/.test(num) && parseInt(num) > 2000) return "";
        return m[1];
    }

    // 判断文本是否是有效的点赞数字
    function isValidLikeText(text) {
        if (!text) return false;
        let t = text.trim();
        if (!t) return false;
        if (!/\d/.test(t)) return false;
        // 点赞数字文本很短(如 "10"、"1.2K"、"1.2万"、"12,345")
        if (t.length > 15) return false;
        // 排除常见的非数字文本
        const exclude = /^(Like|赞|Shared|Share|Dislike|踩|Reply|Save|Download|Clip|Thanks|Report)$/i;
        if (exclude.test(t)) return false;
        return /^[\d.,]+\s*[KkMmBb万]?$/.test(t) || /\d/.test(t);
    }

    // 在指定根节点(可能是shadowRoot)内递归查找点赞数
    function searchInNode(root) {
        if (!root) return "";
        // 1. 在当前root中查找目标span(已知class名)
        const spans = root.querySelectorAll(
            'div.ytSpecButtonShapeWithLabelLabel span, span.ytAttributedStringHost, span.yt-core-attributed-string, .yt-spec-button-shape-next__text, .yt-spec-button-shape-next__button-text-content'
        );
        for (const span of spans) {
            const text = (span.textContent || "").trim();
            if (isValidLikeText(text)) {
                const num = extractNum(text);
                if (num) return num;
            }
        }
        // 2. 降级: 在button元素内查找所有直接文本节点(应对class名变化)
        const btnsInRoot = root.querySelectorAll('button');
        for (const b of btnsInRoot) {
            // 遍历button的直接子节点文本
            for (const node of b.childNodes) {
                if (node.nodeType === Node.TEXT_NODE) {
                    const text = (node.textContent || "").trim();
                    if (isValidLikeText(text)) {
                        const num = extractNum(text);
                        if (num) return num;
                    }
                }
            }
            // 也检查button内所有非icon/非svg元素的文本
            const textEls = b.querySelectorAll('div, span');
            for (const el of textEls) {
                // 跳过icon/svg容器
                if (el.querySelector('svg, yt-icon, yt-animated-icon, lottie-component')) continue;
                const text = (el.textContent || "").trim();
                if (isValidLikeText(text)) {
                    const num = extractNum(text);
                    if (num) return num;
                }
            }
        }
        // 3. 查找button的aria-label
        const btns = root.querySelectorAll(
            'button[aria-label*="like"], button[aria-label*="Like"], button[aria-label*="点赞"]'
        );
        for (const btn of btns) {
            const num = extractFromAriaLabel(btn.getAttribute('aria-label') || '');
            if (num) return num;
        }
        // 4. 递归穿透shadowRoot
        const allEls = root.querySelectorAll('*');
        for (const el of allEls) {
            if (el.shadowRoot) {
                const result = searchInNode(el.shadowRoot);
                if (result) return result;
            }
        }
        return "";
    }

    // 查找点赞按钮宿主元素(按优先级)
    // 优先级1: 在action bar/top-level-buttons容器内查找(精确)
    // 优先级2: 全局查找like-button-view-model
    function findLikeHosts() {
        // Shorts: reel-action-bar-view-model
        const actionBar = document.querySelector(
            'reel-action-bar-view-model, .ytwReelActionBarViewModelHost, #top-level-buttons-computed, #menu-container ytd-menu-renderer'
        );
        if (actionBar) {
            const hosts = actionBar.querySelectorAll(
                'like-button-view-model, .ytLikeButtonViewModelHost, #segmented-like-button'
            );
            if (hosts.length > 0) return hosts;
        }
        // 全局查找(兜底)
        return document.querySelectorAll(
            'like-button-view-model, .ytLikeButtonViewModelHost, #segmented-like-button'
        );
    }

    const likeHosts = findLikeHosts();
    for (const host of likeHosts) {
        // 先在light DOM中查找
        let result = searchInNode(host);
        if (result) return result;
        // 再穿透host自身的shadowRoot查找
        if (host.shadowRoot) {
            result = searchInNode(host.shadowRoot);
            if (result) return result;
        }
    }

    return "0";
})()
`

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
		if (r >= '0' && r <= '9') || r == '.' || r == 'k' || r == 'm' || r == 'b' || r == '万' {
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
	if strings.HasSuffix(s, "万") {
		multiplier = 10000.0
		s = strings.TrimSuffix(s, "万")
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int(val * multiplier)
}

// parseYouTubeSubCount 解析YouTube订阅数，支持多种语言和格式:
// "1.2M subscribers"(英), "12.5万 位订阅者"(中), "1,2 M suscriptores"(欧),
// "1 093 subscribers"(空格千分), "チャンネル登録者数 1.2万人"(日), "1.5億"(亿)
func parseYouTubeSubCount(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 移除空格和不间断空格(部分语言用作千分位分隔符, 如 "1 093")
	s = strings.ReplaceAll(s, "\u00a0", "")
	s = strings.ReplaceAll(s, " ", "")

	// 提取后缀并计算倍率
	multiplier := 1.0
	hasSuffix := true
	switch {
	case strings.HasSuffix(s, "万"):
		multiplier = 10000.0
		s = strings.TrimSuffix(s, "万")
	case strings.HasSuffix(s, "億"):
		multiplier = 100000000.0
		s = strings.TrimSuffix(s, "億")
	case strings.HasSuffix(s, "亿"):
		multiplier = 100000000.0
		s = strings.TrimSuffix(s, "亿")
	default:
		if len(s) > 0 {
			last := strings.ToLower(s[len(s)-1:])
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

	// 处理小数/千分位分隔符:
	// 同时含逗号和句号: 最后出现的是小数点
	// 仅含逗号: 有后缀→小数点(欧洲), 无后缀→千分位(英语)
	// 仅含句号: 视为小数点
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
