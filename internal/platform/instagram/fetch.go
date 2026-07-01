package instagram

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

func FetchInstagramPosts(ctx context.Context, logger *logx.Logger, req scraper.FetchRequest) (scraper.FetchResult, error) {
	logger.Print("INS_FETCH", "开始执行 Instagram Reels 混合采集 (列表提取浏览量 + 详情页提取明细)...")

	silentCtx, silentCancel := chromedp.NewContext(ctx, chromedp.WithErrorf(func(string, ...interface{}) {}))
	defer silentCancel()

	// 1. 导航到主页
	if err := chromedp.Run(silentCtx, chromedp.Navigate(req.SourceURL)); err != nil {
		return scraper.FetchResult{}, err
	}
	time.Sleep(5 * time.Second)

	// 2. 切换至 Reels 视图
	logger.Print("INS_FETCH", "正在定位并切换至 Reels 专属视频视图...")
	reelsTargetSel := `div[role="tablist"] svg[aria-label="Reels"]`

	clickCtx, clickCancel := context.WithTimeout(silentCtx, 6*time.Second)
	err := chromedp.Run(clickCtx, chromedp.Click(reelsTargetSel, chromedp.ByQuery))
	clickCancel()

	if err != nil {
		logger.Print("INS_WARN", "直接点击 SVG 标签失败，尝试点击其外层 Tab 容器...")
		backupCtx, backupCancel := context.WithTimeout(silentCtx, 4*time.Second)
		_ = chromedp.Run(backupCtx, chromedp.Click(`div[role="tablist"] > a[href*="/reels/"]`, chromedp.ByQuery))
		backupCancel()
	}

	// 3. 🌟 极速嗅探与验证：精准定位浏览量 DOM
	type TempReelItem struct {
		Link  string `json:"link"`
		Views string `json:"views"`
	}
	var rawItems []TempReelItem

	logger.Print("INS_FETCH", "正在执行精准 DOM 结构验证...")
	time.Sleep(10 * time.Second)
	err = chromedp.Run(silentCtx, chromedp.Evaluate(`(() => {
		let results = [];
		// 获取 Reels 列表中的所有发文卡片
		let anchors = Array.from(document.querySelectorAll('a[href*="/reel/"]'));
		
		anchors.forEach((a, index) => {
			let viewStr = "0";
			// 1. 根据你提供的特征定位包含 SVG 的外层 div
			let iconWrapper = a.querySelector('div svg[aria-label="View Count Icon"]').parentElement;
			
			if (iconWrapper) {
				// 获取包含数字的父节点（包含图标和数字的那个 html-div）
				let container = iconWrapper.parentElement;
				
				// 打印调试信息到控制台，方便你人工检查
				console.log('Video [' + index + '] Container HTML:', container.innerHTML);
				
				// 2. 这里的 span 应该就在这个 container 内部
				let numSpan = container.querySelector('span.x1lliihq'); 
				if (numSpan) {
					viewStr = numSpan.textContent.trim();
				}
			}
			results.push({ link: a.href, views: viewStr });
		});
		return results;
	})()`, &rawItems))

	if err != nil {
		logger.Print("INS_ERROR", "嗅探脚本执行失败: "+err.Error())
	} else {
		// 打印出来给你人工检查
		for _, item := range rawItems {
			logger.Print("INS_DEBUG", fmt.Sprintf("▶ 发现链接: %s | 解析到的浏览量: %s", item.Link, item.Views))
		}
	}

	// 如果 15 秒后还是空数组，说明被风控或者该账号真的没有 Reels
	if len(rawItems) == 0 {
		return scraper.FetchResult{}, fmt.Errorf("列表页解析失败：长时间未加载出视频节点，可能触发风控或网络超时")
	}

	var posts []scraper.Post

	// 4. 遍历抓取到的链接，进入详情页获取标题、时间、点赞、评论等明细
	for idx, item := range rawItems {
		var data map[string]string

		logger.Print("INS_FETCH", fmt.Sprintf("▶ 正在进入详情页 [%d/%d] (附带浏览量数据: %s)", idx+1, len(rawItems), item.Views))

		detailCtx, detailCancel := chromedp.NewContext(silentCtx)
		timeoutCtx, timeoutCancel := context.WithTimeout(detailCtx, 8*time.Second)

		err := chromedp.Run(timeoutCtx,
			chromedp.Navigate(item.Link),
			chromedp.Sleep(4*time.Second),
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
					"shares": shares
				};
			})()`, &data),
		)

		timeoutCancel()
		detailCancel() // 彻底关闭页面释放内存

		if err != nil {
			logger.Print("INS_WARN", fmt.Sprintf("详情页 [%s] 抓取中断或超时跳过: %v", item.Link, err))
			continue
		}

		// 5. 组合列表页浏览量与详情页数据
		likesCount, _ := strconv.Atoi(data["likes"])
		commentsCount, _ := strconv.Atoi(data["comments"])
		sharesCount, _ := strconv.Atoi(data["shares"])
		viewsCount := parseInsMetric(item.Views) // 清洗 "151" / "2K"

		postItem := scraper.Post{
			Title:       data["title"],
			Link:        item.Link,
			PublishTime: data["time"],
			Likes:       likesCount,
			Comments:    commentsCount,
			Shares:      sharesCount,
			Views:       viewsCount, // 成功落库
		}

		logger.Print("INS_DATA", fmt.Sprintf(
			"✅ 清洗完毕 -> 浏览: %d | 点赞: %d | 评论: %d | 链接: %s",
			postItem.Views, postItem.Likes, postItem.Comments, postItem.Link,
		))

		posts = append(posts, postItem)
		time.Sleep(2 * time.Second) // 循环间歇，防止高频拦截
	}

	logger.Print("INS_FETCH", fmt.Sprintf("Instagram 抓取执行完毕，本次成功收录 %d 条有效数据。", len(posts)))
	return scraper.FetchResult{Posts: posts}, nil
}

// 辅助数字清洗工具
func parseInsMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0
	}
	s = strings.ReplaceAll(s, ",", "")

	multiplier := 1.0
	if strings.HasSuffix(s, "k") {
		multiplier = 1000.0
		s = strings.TrimSuffix(s, "k")
	} else if strings.HasSuffix(s, "m") {
		multiplier = 1000000.0
		s = strings.TrimSuffix(s, "m")
	}

	var clean strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' {
			clean.WriteRune(r)
		}
	}

	val, err := strconv.ParseFloat(clean.String(), 64)
	if err != nil {
		return 0
	}
	return int(val * multiplier)
}
