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

	// ctx已由调用方配置错误抑制，直接使用(避免NewContext创建多余空白标签页)

	// 1. 导航到主页
	if err := chromedp.Run(ctx, chromedp.Navigate(req.SourceURL)); err != nil {
		return scraper.FetchResult{}, err
	}
	time.Sleep(5 * time.Second)

	// 2. 切换至 Reels 视图
	logger.Print("INS_FETCH", "正在定位并切换至 Reels 专属视频视图...")
	reelsTargetSel := `div[role="tablist"] svg[aria-label="Reels"]`

	clickCtx, clickCancel := context.WithTimeout(ctx, 6*time.Second)
	err := chromedp.Run(clickCtx, chromedp.Click(reelsTargetSel, chromedp.ByQuery))
	clickCancel()

	if err != nil {
		logger.Print("INS_WARN", "直接点击 SVG 标签失败，尝试点击其外层 Tab 容器...")
		backupCtx, backupCancel := context.WithTimeout(ctx, 4*time.Second)
		_ = chromedp.Run(backupCtx, chromedp.Click(`div[role="tablist"] > a[href*="/reels/"]`, chromedp.ByQuery))
		backupCancel()
	}

	// 2.5 提取粉丝数和发帖数：以main[role="main"] > header为根容器
	var totalFollowers int
	var totalPosts int
	time.Sleep(3 * time.Second) // 等待页面稳定
	type statsResult struct {
		PostsText     string `json:"posts_text"`
		FollowersText string `json:"followers_text"`
		FollowingText string `json:"following_text"`
	}
	var stats statsResult
	statsJS := `(() => {
		// 定位唯一根容器: main[role="main"] > header
		const header = document.querySelector('main[role="main"] header');
		if (!header) return {posts_text: '', followers_text: '', following_text: ''};

		let postsText = '';
		let followersText = '';
		let followingText = '';

		// 多语言关键词映射
		const postsRe = /\bposts?\b|貼文|帖子|个帖子|個貼文|篇贴文|投稿|publicaciones?|publicações?|beiträge|投稿記事|게시물|публикац/i;
		const followersRe = /\bfollowers?\b|粉丝|粉絲|follower|abonnés?|abonnenten|seguidores?|seguaci|フォロワー|팔로워|подписчик/i;
		const followingRe = /\bfollowing\b|关注中|已关注|abonnements?|folgt|seguidos?|seguendo|フォロー中|팔로잉|подписок/i;

		// Helper: 从stat容器中提取数字
		// 从span.html-span向上查找带title属性的祖先(不跨兄弟元素), 否则使用html-span文本
		function extractNum(container) {
			const htmlSpan = container.querySelector('span.html-span');
			if (!htmlSpan) return '';
			// 从htmlSpan向上遍历, 查找带title属性的祖先节点(在container范围内)
			let node = htmlSpan;
			for (let i = 0; i < 5; i++) {
				if (!node || node === container) break;
				if (node.getAttribute && node.getAttribute('title')) {
					return node.getAttribute('title');
				}
				node = node.parentElement;
			}
			// 最后检查container本身
			if (container.getAttribute && container.getAttribute('title')) {
				return container.getAttribute('title');
			}
			return (htmlSpan.textContent || '').trim();
		}

		// 策略1: 以span[dir="auto"]为stat容器(每个统计项独立一个dir=auto span)
		// 例: <span dir="auto"><span class="x5n08af"><span class="html-span">57</span></span> posts</span>
		const dirSpans = header.querySelectorAll('span[dir="auto"]');
		for (const span of dirSpans) {
			if (!span.querySelector('span.html-span')) continue;
			const text = (span.textContent || '').trim().toLowerCase();
			if (!/\d/.test(text)) continue;
			const numText = extractNum(span);
			if (!numText || !/\d/.test(numText)) continue;

			if (!postsText && postsRe.test(text)) {
				postsText = numText;
			} else if (!followersText && followersRe.test(text)) {
				followersText = numText;
			} else if (!followingText && followingRe.test(text)) {
				followingText = numText;
			}
		}

		// 策略2: 兜底 - 查找li/a元素(部分布局使用ul>li结构)
		if (!postsText || !followersText || !followingText) {
			const candidates = header.querySelectorAll('li, a');
			for (const el of candidates) {
				// 跳过不含html-span的元素(非stat项)
				if (!el.querySelector('span.html-span')) continue;
				const text = (el.textContent || '').trim().toLowerCase();
				if (!/\d/.test(text)) continue;

				// 限定el为直接stat容器: 检查el的直接子span或el本身
				const statContainer = el.querySelector('span[dir="auto"]') || el;
				const numText = extractNum(statContainer);
				if (!numText || !/\d/.test(numText)) continue;

				if (!postsText && postsRe.test(text)) {
					postsText = numText;
				} else if (!followersText && followersRe.test(text)) {
					followersText = numText;
				} else if (!followingText && followingRe.test(text)) {
					followingText = numText;
				}
			}
		}

		// 策略3: 最终兜底 - 遍历header中所有含html-span的元素, 按最近的关键词祖先匹配
		if (!postsText || !followersText || !followingText) {
			const htmlSpans = header.querySelectorAll('span.html-span');
			for (const hs of htmlSpans) {
				// 向上找最近的含关键词文本的祖先(最多5层)
				let ancestor = hs.parentElement;
				let matchText = '';
				let foundKey = '';
				for (let i = 0; i < 6 && ancestor; i++) {
					const at = (ancestor.textContent || '').trim().toLowerCase();
					if (!foundKey && postsRe.test(at)) { foundKey = 'posts'; matchText = at; }
					else if (!foundKey && followersRe.test(at)) { foundKey = 'followers'; matchText = at; }
					else if (!foundKey && followingRe.test(at)) { foundKey = 'following'; matchText = at; }
					// 找到关键词后再向上1层确认是否是独立stat容器(不含其他关键词)
					if (foundKey) {
						// 检查这个祖先是否同时包含多个关键词(说明是外层大容器), 如是则继续向上
						const hasPosts = postsRe.test(at);
						const hasFollowers = followersRe.test(at);
						const hasFollowing = followingRe.test(at);
						const keyCount = [hasPosts, hasFollowers, hasFollowing].filter(Boolean).length;
						if (keyCount === 1) break; // 只含一个关键词, 是精确容器
					}
					ancestor = ancestor.parentElement;
				}
				if (!foundKey) continue;

				// 提取数字: 向上找title
				let node = hs;
				let numText = '';
				for (let i = 0; i < 5; i++) {
					if (!node) break;
					if (node.getAttribute && node.getAttribute('title')) {
						numText = node.getAttribute('title');
						break;
					}
					node = node.parentElement;
				}
				if (!numText) numText = (hs.textContent || '').trim();
				if (!numText || !/\d/.test(numText)) continue;

				if (foundKey === 'posts' && !postsText) postsText = numText;
				else if (foundKey === 'followers' && !followersText) followersText = numText;
				else if (foundKey === 'following' && !followingText) followingText = numText;
			}
		}

		return {posts_text: postsText, followers_text: followersText, following_text: followingText};
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(statsJS, &stats)); err == nil {
		if stats.FollowersText != "" {
			totalFollowers = parseInsMetric(stats.FollowersText)
			logger.Print("INS_FETCH", fmt.Sprintf("账号总粉丝数: %d (原始: %s)", totalFollowers, stats.FollowersText))
		} else {
			logger.Print("INS_FETCH", "INS_FETCH_SUB 未在header中找到粉丝数(followers)")
		}
		if stats.PostsText != "" {
			totalPosts = parseInsMetric(stats.PostsText)
			logger.Print("INS_FETCH", fmt.Sprintf("账号总发帖数: %d (原始: %s)", totalPosts, stats.PostsText))
		} else {
			logger.Print("INS_FETCH", "INS_FETCH_POST 未在header中找到发帖数(posts)")
		}
		if stats.FollowingText != "" {
			logger.Print("INS_FETCH", fmt.Sprintf("账号关注数(following): %s (仅记录不入库)", stats.FollowingText))
		}
	} else {
		logger.Print("INS_WARN", fmt.Sprintf("提取粉丝/发帖数脚本执行失败: %v", err))
	}

	// 3. 🌟 极速嗅探与验证：精准定位浏览量 DOM
	type TempReelItem struct {
		Link  string `json:"link"`
		Views string `json:"views"`
	}
	var rawItems []TempReelItem

	logger.Print("INS_FETCH", "正在执行精准 DOM 结构验证...")
	time.Sleep(10 * time.Second)
	err = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
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

		detailCtx, detailCancel := chromedp.NewContext(ctx)
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
	result := scraper.FetchResult{
		Posts:          posts,
		TotalFollowers: totalFollowers,
		TotalPosts:     totalPosts,
	}
	return scraper.SanitizeResult(result), nil
}

// parseInsMetric 解析Instagram数字，支持:
// "53", "1,234", "12.5K", "1.5M", "1.2万", "1 234"(空格千分), "1,2K"(欧洲小数)
func parseInsMetric(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0
	}
	// 移除空格和不间断空格(部分语言用作千分位)
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

	// 处理千分位/小数分隔符:
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
