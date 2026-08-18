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

	// ctx已由调用方配置错误抑制，直接使用(避免NewContext创建多余空白标签页)

	// 1. 强行导航至后台管理页
	targetURL := "https://www.tiktok.com/tiktokstudio/content"
	logger.Print("TT_FETCH", "正在导航至创作后台: "+targetURL)
	if err := chromedp.Run(ctx, chromedp.Navigate(targetURL)); err != nil {
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

		// 格式化 TikTok 的日期为 "YYYY-MM-DD HH:mm:ss"，支持多语言月份
		function formatTikTokDate(rawDateStr) {
			if (!rawDateStr) return "";
			// 清洗特殊窄空格
			let cleanStr = rawDateStr.replace(/[\u2000-\u206F\u2070-\u209F\u20A0-\u20CF\u20D0-\u20FF\u2100-\u214F]/g, " ").trim();
			cleanStr = cleanStr.replace(/\s+/g, " ").trim();

			// 多语言月份映射 (与YouTube一致)
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
			};

			let currentYear = new Date().getFullYear();
			let pad = (n) => n < 10 ? '0' + n : String(n);

			// 1. 先尝试Date.parse(补当前年份)
			let finalStr = currentYear + " " + cleanStr;
			try {
				let ts = Date.parse(finalStr);
				if (!isNaN(ts)) {
					let d = new Date(ts);
					return d.getFullYear() + '-' + pad(d.getMonth()+1) + '-' + pad(d.getDate()) + ' ' +
					       pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
				}
			} catch(e) {}

			// 2. 多语言月份解析
			let normalized = cleanStr.toLowerCase();
			normalized = normalized.normalize ? normalized.normalize('NFD').replace(/[\u0300-\u036f]/g, '') : normalized;

			let foundMonth = 0, matchedLen = 0;
			let monthKeys = Object.keys(monthMap).sort((a,b) => b.length - a.length);
			for (const key of monthKeys) {
				let keyNorm = key.normalize ? key.normalize('NFD').replace(/[\u0300-\u036f]/g, '') : key;
				let re = new RegExp('(^|[^a-zäöüışğç0-9])' + keyNorm.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '($|[^a-zäöüışğç0-9])', 'i');
				if (re.test(normalized) && key.length > matchedLen) {
					foundMonth = monthMap[key];
					matchedLen = key.length;
				}
			}

			if (foundMonth > 0) {
				// 提取时间 HH:MM 或 H:MM AM/PM
				let hours = 0, minutes = 0;
				let timeMatch = cleanStr.match(/(\d{1,2}):(\d{2})(?::(\d{2}))?\s*(am|pm|上午|下午|오전|오후)?/i);
				if (timeMatch) {
					hours = parseInt(timeMatch[1], 10);
					minutes = parseInt(timeMatch[2], 10);
					let ampm = (timeMatch[4] || '').toLowerCase();
					if ((ampm === 'pm' || ampm === '下午' || ampm === '오후') && hours < 12) hours += 12;
					if ((ampm === 'am' || ampm === '上午' || ampm === '오전') && hours === 12) hours = 0;
				}

				// 提取日和年
				let nums = cleanStr.match(/\d{1,4}/g);
				let day = 1, year = currentYear;
				if (nums) {
					for (const n of nums.map(n => parseInt(n,10))) {
						if (n >= 1970 && n <= 2100) { year = n; }
						// 避免把时间的小时/分钟误认为日
						else if (n >= 1 && n <= 31) {
							// 检查这个数字是否是时间部分(后面跟冒号)
							let numStr = String(n);
							let idx = cleanStr.indexOf(numStr);
							let after = cleanStr.substring(idx + numStr.length).trim();
							if (!after.startsWith(':')) {
								day = n;
							}
						}
					}
				}

				return year + '-' + pad(foundMonth) + '-' + pad(day) + ' ' + pad(hours) + ':' + pad(minutes) + ':00';
			}

			return "";
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

	if err := chromedp.Run(ctx, chromedp.Evaluate(runScrollScript, nil)); err != nil {
		return scraper.FetchResult{}, fmt.Errorf("inject script failed: %w", err)
	}

	// 4. 等待浏览器信号
	var isDone bool
	for i := 0; i < 20; i++ {
		_ = chromedp.Run(ctx, chromedp.Evaluate("window._ttScrollDone || false", &isDone))
		if isDone {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// 5. 捞回干净数据
	var jsResult []map[string]string
	if err := chromedp.Run(ctx, chromedp.Evaluate("window._ttPostsData || []", &jsResult)); err != nil {
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

	// 7. 导航到个人主页提取总粉丝数和总点赞量
	logger.Print("TT_FETCH", "正在导航到个人主页提取粉丝数和点赞量...")
	if err := chromedp.Run(ctx, chromedp.Navigate("https://www.tiktok.com/profile")); err != nil {
		logger.Print("TT_WARN", fmt.Sprintf("导航到个人主页失败: %v", err))
		return scraper.SanitizeResult(scraper.FetchResult{Posts: posts}), nil
	}
	time.Sleep(5 * time.Second)

	var followersStr, likesStr string
	_ = chromedp.Run(ctx,
		chromedp.Text(`strong[data-e2e="followers-count"]`, &followersStr, chromedp.ByQuery),
		chromedp.Text(`strong[data-e2e="likes-count"]`, &likesStr, chromedp.ByQuery),
	)

	totalFollowers := parseTikTokMetric(followersStr)
	totalLikes := parseTikTokMetric(likesStr)
	logger.Print("TT_FETCH", fmt.Sprintf("账号总粉丝数: %d, 总点赞量: %d", totalFollowers, totalLikes))

	// 上报账号统计数据(TikTok不提取totalPosts,传0),失败不中断流程
	scraper.ReportAccountStats(ctx, logger, req.AccountID, totalFollowers, 0, req.AccountStatsEndpoint)

	return scraper.SanitizeResult(scraper.FetchResult{
		Posts:         posts,
		TotalFollowers: totalFollowers,
		TotalLikes:     totalLikes,
	}), nil
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
