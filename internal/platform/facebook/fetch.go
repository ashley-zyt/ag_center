package facebook

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"

	"github.com/chromedp/chromedp"
)

const (
	fbScrollTimes = 5   // 连续滚动次数
	fbScrollPx    = 500 // 每次滚动像素
)

// fbTimeOffsetSeconds 返回本机时钟相对真实时间的偏移(秒)，用于把 Facebook 的
// 相对时间("3 days ago")换算成绝对日期时校正"现在"。
// 可通过环境变量 FB_TIME_OFFSET_SECONDS 覆盖：本机慢 15 小时则设 54000。
func fbTimeOffsetSeconds() int64 {
	if v := os.Getenv("FB_TIME_OFFSET_SECONDS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// FetchFacebookPosts 抓取 Facebook 个人主页的发文。
// 打开主页 -> 等待加载 -> 连续向下滚动 5 次(每次 500px)触发懒加载 -> 一次性采集所有发文。
func FetchFacebookPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("FB_FETCH", "启用防御性抓取模式: "+req.SourceURL)

	if err := chromedp.Run(ctx, chromedp.Navigate(req.SourceURL)); err != nil {
		return scraper.FetchResult{}, err
	}
	time.Sleep(8 * time.Second)

	// 先连续向下滚动 N 次 × 500px，把懒加载的发文都加载出来，再采集
	for i := 0; i < fbScrollTimes; i++ {
		_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`window.scrollBy(0, %d);`, fbScrollPx), nil))
		time.Sleep(3 * time.Second)
	}

	// 预热：逐个滚动到每个帖子位置并停留，触发浏览量/评论数的异步渲染。
	// Facebook 的浏览量是异步统计的，帖子只有进入视口并停留足够时间才会加载出数字；
	// scrollBy 快速掠过时浏览量 API 来不及返回，导致采集到 0。
	var anchorCount int
	_ = chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('[data-ad-rendering-role="profile_name"]').length`, &anchorCount))
	preheatCount := anchorCount
	if preheatCount > 10 {
		preheatCount = 10
	}
	logger.Print("FB_FETCH", fmt.Sprintf("检测到 %d 个发文，仅预热前 %d 个触发浏览量渲染", anchorCount, preheatCount))
	for i := 0; i < preheatCount; i++ {
		_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`document.querySelectorAll('[data-ad-rendering-role="profile_name"]')[%d].scrollIntoView({block:'center'});`, i), nil))
		time.Sleep(1500 * time.Millisecond)
	}
	// 预热后滚回顶部再重新向下滚动，把虚拟列表回收掉的帖子重新加载回来。
	// Facebook feed 是虚拟列表：scrollIntoView 逐个滚到底部后，顶部帖子会被卸载；
	// 浏览量此刻已通过预热渲染进 store 缓存，重新滚动后帖子会带着浏览量立即出现。
	_ = chromedp.Run(ctx, chromedp.Evaluate(`window.scrollTo(0, 0);`, nil))
	time.Sleep(2 * time.Second)
	for i := 0; i < fbScrollTimes; i++ {
		_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`window.scrollBy(0, %d);`, fbScrollPx), nil))
		time.Sleep(2 * time.Second)
	}

	// 末尾等待，让浏览量从缓存重新渲染
	time.Sleep(6 * time.Second)
	logger.Print("FB_FETCH", fmt.Sprintf("已完成连续滚动 %d 次并预热浏览量，开始采集", fbScrollTimes))

	scrapeJS := `(() => {
		// 1. 收集页面隐藏区里的时间文本(按文档顺序)。
		//    Facebook 把每条发文的可访问性时间("3 days ago"/"Just now"/"September 8, 2026")
		//    放在底部 hidden 容器或 <svg><text> 里，顺序与发文出现顺序一致，可用索引与卡片对应。
		let timeTexts = [];
		let timeDebug = [];
		let timeSeen = new Set();
		let timeRe = /^(\d+\s+(seconds?|minutes?|hours?|days?|weeks?|months?|years?)\s+ago|just now|yesterday|today\s+at\s+\d{1,2}:\d{2}|yesterday\s+at\s+\d{1,2}:\d{2}|(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?(?:\s+at\s+\d{1,2}:\d{2}(?:\s*(?:am|pm))?)?(?:,?\s*\d{4})?)$/i;
		document.querySelectorAll('div[hidden="true"] span, div[style*="-10000px"] text, svg text').forEach((el) => {
			let t = (el.textContent || '').trim();
			if (!t) return;
			if (timeDebug.indexOf(t) === -1) timeDebug.push(t);
			if (timeRe.test(t) && !timeSeen.has(t.toLowerCase())) {
				timeSeen.add(t.toLowerCase());
				timeTexts.push(t);
			}
		});

		// 2. 以每帖唯一的 author 标记 data-ad-rendering-role="profile_name" 为锚点，
		//    向上找到同时包含点赞/评论/转发按钮(或正文/视频)的最近祖先，即为整张卡片。
		let cards = [];
		let anchors = document.querySelectorAll('[data-ad-rendering-role="profile_name"]');
		anchors.forEach((anchor) => {
			if (cards.length >= 10) return;
			let node = anchor;
			while (node && node !== document.body) {
				if (node.querySelector('[aria-label^="Like:"], [aria-label^="Comment on"], [aria-label^="Send this"], [data-ad-rendering-role="story_message"], [data-video-id]')) {
					cards.push(node);
					break;
				}
				node = node.parentElement;
			}
		});
		let seen = new Set();
		cards = cards.filter((c) => { if (seen.has(c)) return false; seen.add(c); return true; });

		// 从整个卡片提取"数字 + 图标"对：眼睛图标→浏览量，评论图标→评论数。
		// 注意 role="toolbar"("See who reacted") 只包住点赞，不含浏览/评论，所以不能以它作范围。
		function extractStats(card) {
			let views = 0, comments = 0;
			let numTexts = [];
			// 图标数量统计：眼睛图标(path 含 4.75) 和 评论图标(css-img)
			let html = card.innerHTML || '';
			let eyeCount = (html.match(/4\.75/g) || []).length;
			let cIconCount = (html.match(/data-visualcompletion="css-img"/g) || []).length;
			function parseCount(s) {
				s = s.replace(/,/g, '').trim().toUpperCase();
				if (s.endsWith('K')) return Math.round(parseFloat(s) * 1000);
				if (s.endsWith('M')) return Math.round(parseFloat(s) * 1000000);
				let n = parseInt(s, 10);
				return isNaN(n) ? 0 : n;
			}
			card.querySelectorAll('span').forEach((s) => {
				if (s.children.length > 0) return; // 只要叶子节点，避免重复计数
				let t = (s.innerText || '').trim();
				if (!/\d/.test(t)) return; // 含数字即可
				numTexts.push(t);
				// 新格式(现行)：数字 + 文字单位，如 "83 views" / "2 comments"
				let vm = t.match(/^([\d,]+(?:\.\d+)?[KkMm]?)\s*(?:views?|次观看|次浏览|次播放)$/i);
				if (vm) { views = parseCount(vm[1]); return; }
				let cm = t.match(/^([\d,]+(?:\.\d+)?[KkMm]?)\s*(?:comments?|条评论|条回复)$/i);
				if (cm) { comments = parseCount(cm[1]); return; }
				// 旧格式(兼容)：纯数字 + 图标
				if (!/^\d[\d,]*$/.test(t)) return;
				let val = parseInt(t.replace(/,/g, ''), 10);
				let node = s.parentElement;
				for (let i = 0; i < 3 && node; i++) {
					let next = node.nextElementSibling;
					if (next) {
						// 眼睛图标：容器内含内联 svg(path 含 4.75)
						if (next.querySelector('svg') && next.innerHTML.indexOf('4.75') !== -1) { views = val; return; }
						// 评论图标：容器内含 css 背景图 <i>
						if (next.querySelector('i[data-visualcompletion="css-img"]')) { comments = val; return; }
					}
					node = node.parentElement;
				}
			});
			let debug = 'nums=[' + numTexts.join(',') + '] eyes=' + eyeCount + ' cIcons=' + cIconCount;
			return { views: views, comments: comments, debug: debug };
		}

		// 3. 逐卡片提取字段
		let list = [];
		cards.forEach((card, i) => {
			// 3.1 标题(正文)
			let titleEl = card.querySelector('div[data-ad-rendering-role="story_message"]');
			let title = titleEl ? titleEl.innerText.trim() : '';

			// 3.2 链接：视频发文用 data-video-id 拼 /reel/<id>，否则找固定链接
			let link = '';
			let videoEl = card.querySelector('[data-video-id]');
			if (videoEl) {
				let vid = videoEl.getAttribute('data-video-id');
				if (vid) link = 'https://www.facebook.com/reel/' + vid;
			}
			if (!link) {
				let a = card.querySelector('a[href*="/reel/"], a[href*="/posts/"], a[href*="/photo"], a[href*="/videos/"], a[href*="/watch/"]');
				if (a) link = a.href;
			}

			// 3.3 相对时间(按序匹配隐藏区时间文本)
			let timeText = i < timeTexts.length ? timeTexts[i] : '';

			// 3.4 点赞数：aria-label="Like: N person(s)"
			let likes = 0;
			let likeEl = card.querySelector('[aria-label^="Like:"]');
			if (likeEl) {
				let m = (likeEl.getAttribute('aria-label') || '').match(/(\d+)/);
				if (m) likes = parseInt(m[1], 10);
			}

			// 3.5 浏览量 + 评论数：从反应汇总区按图标类型识别
			let stats = extractStats(card);

			// 3.6 转发数
			let shares = 0;
			let shareBtn = card.querySelector('[aria-label^="Send this to"]');
			if (shareBtn) {
				let m = (shareBtn.innerText || '').match(/(\d+)/);
				if (m) shares = parseInt(m[1], 10);
			}

			list.push({title: title, link: link, time: timeText, likes: likes, views: stats.views, comments: stats.comments, shares: shares, debug: stats.debug});
		});
		return {list: list, timeDebug: timeDebug};
	})()`

	type rawPost struct {
		Title    string `json:"title"`
		Link     string `json:"link"`
		Time     string `json:"time"`
		Likes    int    `json:"likes"`
		Views    int    `json:"views"`
		Comments int    `json:"comments"`
		Shares   int    `json:"shares"`
		Debug    string `json:"debug"`
	}

	var scrapeResult struct {
		List      []rawPost `json:"list"`
		TimeDebug []string  `json:"timeDebug"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(scrapeJS, &scrapeResult)); err != nil {
		return scraper.FetchResult{}, err
	}
	results := scrapeResult.List
	if len(scrapeResult.TimeDebug) > 0 {
		logger.Print("FB_DEBUG", fmt.Sprintf("隐藏区时间文本(%d 条): %s", len(scrapeResult.TimeDebug), strings.Join(scrapeResult.TimeDebug, " | ")))
	} else {
		logger.Print("FB_DEBUG", "隐藏区未找到任何时间文本(新帖子可能尚未生成可访问性时间)")
	}

	// 用校正后的"现在"来换算相对时间(补偿本机时钟偏差)
	now := time.Now().Add(time.Duration(fbTimeOffsetSeconds()) * time.Second)
	var posts []scraper.Post
	for _, item := range results {
		if item.Title == "" && item.Link == "" {
			continue // 跳过空卡片
		}
		absTime := resolveRelativeTime(item.Time, now)
		posts = append(posts, scraper.Post{
			Title:       item.Title,
			Link:        item.Link,
			PublishTime: absTime,
			Likes:       item.Likes,
			Comments:    item.Comments,
			Shares:      item.Shares,
			Views:       item.Views,
		})
		logger.Print("FB_DATA", fmt.Sprintf("[时间:%s][赞:%d][浏览:%d][评论:%d][转发:%d] %s (%s)", absTime, item.Likes, item.Views, item.Comments, item.Shares, item.Title, item.Link))
		if item.Views == 0 && item.Comments == 0 && item.Debug != "" {
			logger.Print("FB_DEBUG", fmt.Sprintf("浏览/评论均为0，卡片内纯数字节点: [%s]", item.Debug))
		}
	}

	logger.Print("FB_FETCH", fmt.Sprintf("采集完成: 共 %d 条发文", len(posts)))
	return scraper.SanitizeResult(scraper.FetchResult{Posts: posts}), nil
}

// absDateRe 匹配绝对日期，如 "september 8, 2026" / "sept 8" / "august 31 at 7:28 am"。
var absDateRe = regexp.MustCompile(`^(jan|feb|mar|apr|may|jun|jul|aug|sep|sept|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:\s+at\s+(\d{1,2}):(\d{2})(?:\s*(am|pm))?)?(?:,?\s*(\d{4}))?$`)

// monthNum 把月份缩写转成数字。
func monthNum(s string) int {
	switch s {
	case "jan":
		return 1
	case "feb":
		return 2
	case "mar":
		return 3
	case "apr":
		return 4
	case "may":
		return 5
	case "jun":
		return 6
	case "jul":
		return 7
	case "aug":
		return 8
	case "sep", "sept":
		return 9
	case "oct":
		return 10
	case "nov":
		return 11
	case "dec":
		return 12
	}
	return 0
}

// parseHM 解析 "15:04" 形式的时间。
func parseHM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad time %q", s)
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("bad time %q", s)
	}
	return h, m, nil
}

// resolveRelativeTime 把 Facebook 的时间文本转换成标准时间字符串。
// 支持: "just now" / "yesterday" / "N seconds|minutes|hours|days|weeks|months|years ago"
// 以及 "today at HH:MM" / "yesterday at HH:MM" / "Month D, YYYY"。
// 相对时间本身有精度损失(如 "3 days ago" 实际可能是 3~4 天前)，这里按 N 个单位整点换算，
// 结果日期最多偏差约 1 天。
func resolveRelativeTime(rel string, now time.Time) string {
	rel = strings.TrimSpace(strings.ToLower(rel))
	// 归一化 Unicode 空白(窄不间断空格 U+202F、不间断空格 U+00A0)为普通空格，
	// Facebook 的时间文本里常混入这些字符导致正则 \s 匹配不到。
	rel = strings.NewReplacer("\u202f", " ", "\u00a0", " ").Replace(rel)
	if rel == "" {
		return ""
	}

	// 绝对日期: "september 8, 2026" / "sept 8" / "august 31 at 7:28 am"
	if m := absDateRe.FindStringSubmatch(rel); m != nil {
		month := monthNum(m[1])
		day, _ := strconv.Atoi(m[2])
		year := now.Year()
		if m[6] != "" {
			year, _ = strconv.Atoi(m[6])
		}
		hour, minute := 0, 0
		if m[3] != "" {
			hour, _ = strconv.Atoi(m[3])
			minute, _ = strconv.Atoi(m[4])
			if strings.EqualFold(m[5], "pm") && hour < 12 {
				hour += 12
			}
			if strings.EqualFold(m[5], "am") && hour == 12 {
				hour = 0
			}
		}
		if month >= 1 && month <= 12 && day >= 1 && day <= 31 {
			return time.Date(year, time.Month(month), day, hour, minute, 0, 0, now.Location()).Format("2006-01-02 15:04:05")
		}
		return ""
	}

	// "today at HH:MM" / "yesterday at HH:MM"
	if strings.HasPrefix(rel, "today at ") {
		if h, mi, err := parseHM(strings.TrimPrefix(rel, "today at ")); err == nil {
			return time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location()).Format("2006-01-02 15:04:05")
		}
		return ""
	}
	if strings.HasPrefix(rel, "yesterday at ") {
		if h, mi, err := parseHM(strings.TrimPrefix(rel, "yesterday at ")); err == nil {
			y := now.Add(-24 * time.Hour)
			return time.Date(y.Year(), y.Month(), y.Day(), h, mi, 0, 0, now.Location()).Format("2006-01-02 15:04:05")
		}
		return ""
	}

	var d time.Duration
	matched := false

	switch {
	case rel == "just now":
		d = 0
		matched = true
	case rel == "yesterday":
		d = -24 * time.Hour
		matched = true
	default:
		if strings.HasSuffix(rel, " ago") {
			fields := strings.Fields(rel) // ["3", "days", "ago"]
			if len(fields) >= 2 {
				if n, err := strconv.Atoi(fields[0]); err == nil {
					unit := fields[1]
					switch {
					case strings.HasPrefix(unit, "second"):
						d = -time.Duration(n) * time.Second
					case strings.HasPrefix(unit, "minute"):
						d = -time.Duration(n) * time.Minute
					case strings.HasPrefix(unit, "hour"):
						d = -time.Duration(n) * time.Hour
					case strings.HasPrefix(unit, "day"):
						d = -time.Duration(n) * 24 * time.Hour
					case strings.HasPrefix(unit, "week"):
						d = -time.Duration(n) * 7 * 24 * time.Hour
					case strings.HasPrefix(unit, "month"):
						d = -time.Duration(n) * 30 * 24 * time.Hour
					case strings.HasPrefix(unit, "year"):
						d = -time.Duration(n) * 365 * 24 * time.Hour
					default:
						return ""
					}
					matched = true
				}
			}
		}
	}

	if !matched {
		return ""
	}
	return now.Add(d).Format("2006-01-02 15:04:05")
}
