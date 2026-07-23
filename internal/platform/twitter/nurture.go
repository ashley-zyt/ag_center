package twitter

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/nurture"

	"github.com/chromedp/chromedp"
)

type twitterActions struct {
	logger *logx.Logger
	ctx    context.Context
}

func (a *twitterActions) HomeURL() string {
	return "https://x.com/home"
}

func (a *twitterActions) Tag() string {
	return "TW_NURTURE"
}

func (a *twitterActions) CheckLogin(ctx context.Context) (string, error) {
	var status string
	checkLoginJs := `(function(){
		var homeFeed = document.querySelector('div[data-testid="primaryColumn"]');
		var loginForm = document.querySelector('form[action="/login"]');
		if (homeFeed) return 'logged_in';
		if (loginForm) return 'not_logged_in';
		return 'abnormal';
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(checkLoginJs, &status)); err != nil {
		return "", err
	}
	return status, nil
}

func (a *twitterActions) IsAd(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *twitterActions) LikePost(ctx context.Context) error {
	likeJs := `(function(){
		var likeBtn = document.querySelector('[data-testid="like"]');
		if(!likeBtn) return false;
		likeBtn.click();
		return true;
	})()`
	var liked bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(likeJs, &liked)); err != nil {
		return err
	}
	if liked {
		a.logger.Print(a.Tag(), "点赞成功")
	} else {
		a.logger.Print(a.Tag(), "未找到点赞按钮")
	}
	return nil
}

func (a *twitterActions) BrowseComments(ctx context.Context) error {
	openCommentsJs := `(function(){
		var commentBtn = document.querySelector('[data-testid="reply"]');
		if(!commentBtn) return false;
		commentBtn.click();
		return true;
	})()`
	var opened bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(openCommentsJs, &opened)); err != nil {
		return err
	}
	if !opened {
		a.logger.Print(a.Tag(), "未找到评论按钮")
		return nil
	}
	a.logger.Print(a.Tag(), "已打开评论区")
	time.Sleep(3 * time.Second)

	browseDuration := time.Duration(rand.Intn(6)+10) * time.Second
	endTime := time.Now().Add(browseDuration)
	for time.Now().Before(endTime) {
		scrollAmount := rand.Intn(300) + 100
		scrollJs := fmt.Sprintf("window.scrollBy(0, %d)", scrollAmount)
		if err := chromedp.Run(ctx, chromedp.Evaluate(scrollJs, nil)); err != nil {
			return err
		}
		time.Sleep(time.Duration(rand.Intn(3)+2) * time.Second)
	}

	closeCommentsJs := `(function(){
		var closeBtn = document.querySelector('[data-testid="x"]');
		if(closeBtn) closeBtn.click();
	})()`
	_ = chromedp.Run(ctx, chromedp.Evaluate(closeCommentsJs, nil))
	a.logger.Print(a.Tag(), "已关闭评论区")
	return nil
}

func (a *twitterActions) FollowUser(ctx context.Context) error {
	openProfileJs := `(function(){
		var userLink = document.querySelector('[data-testid="User-Name"] a');
		if(!userLink) return "";
		userLink.click();
		return userLink.href;
	})()`
	var profileURL string
	if err := chromedp.Run(ctx, chromedp.Evaluate(openProfileJs, &profileURL)); err != nil {
		return err
	}
	if profileURL == "" {
		a.logger.Print(a.Tag(), "未找到用户链接")
		return nil
	}
	a.logger.Print(a.Tag(), "进入用户主页: "+profileURL)
	time.Sleep(5 * time.Second)

	followJs := `(function(){
		var followBtn = document.querySelector('[data-testid="userActions"] button');
		if(!followBtn) return false;
		if(followBtn.innerText && followBtn.innerText.includes('Following')) return false;
		followBtn.click();
		return true;
	})()`
	var followed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(followJs, &followed)); err != nil {
		return err
	}
	if followed {
		a.logger.Print(a.Tag(), "关注成功")
	} else {
		a.logger.Print(a.Tag(), "未找到关注按钮或已关注")
	}
	time.Sleep(5 * time.Second)

	_ = chromedp.Run(ctx, chromedp.Navigate("https://x.com/home"))
	time.Sleep(3 * time.Second)
	a.logger.Print(a.Tag(), "已返回首页")
	return nil
}

func (a *twitterActions) NextPost(ctx context.Context) error {
	scrollAmount := rand.Intn(600) + 300
	scrollJs := fmt.Sprintf("window.scrollBy(0, %d)", scrollAmount)
	if err := chromedp.Run(ctx, chromedp.Evaluate(scrollJs, nil)); err != nil {
		return err
	}
	return nil
}

func (a *twitterActions) MaxWatchSeconds() int {
	return 0
}

func (a *twitterActions) RecoveryURL() string {
	return ""
}

func (a *twitterActions) CheckPageError(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *twitterActions) PreNurture(ctx context.Context) error {
	return nil
}

func NurtureAccount(ctx context.Context, logger *logx.Logger, req nurture.NurtureRequest) (nurture.NurtureResult, error) {
	actions := &twitterActions{
		logger: logger,
		ctx:    ctx,
	}
	tag := actions.Tag()
	silentCtx, silentCancel := chromedp.NewContext(ctx,
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	defer silentCancel()

	// 1. 打开首页
	homeURL := actions.HomeURL()
	logger.Print(tag, "正在导航至首页: "+homeURL)
	if err := chromedp.Run(silentCtx, chromedp.Navigate(homeURL)); err != nil {
		return nurture.NurtureResult{Status: "failed", ErrorInfo: "导航失败: " + err.Error()}, fmt.Errorf("navigate failed: %w", err)
	}
	time.Sleep(8 * time.Second)

	// 2. 检查登录状态
	loginStatus, err := actions.CheckLogin(silentCtx)
	if err != nil {
		return nurture.NurtureResult{Status: "failed", ErrorInfo: "检查登录失败: " + err.Error()}, fmt.Errorf("check login failed: %w", err)
	}
	if loginStatus == "not_logged_in" {
		logger.Print(tag, "检测到账号未登录")
		return nurture.NurtureResult{Status: "not_logged_in", ErrorInfo: "账号未登录"}, fmt.Errorf("账号未登录")
	}
	if loginStatus == "abnormal" {
		logger.Print(tag, "账号状态异常，需人工检查")
		return nurture.NurtureResult{Status: "abnormal", ErrorInfo: "账号状态异常，需人工检查"}, fmt.Errorf("账号状态异常")
	}
	logger.Print(tag, "登录状态检查通过")

	// 3. 随机生成养号目标
	nurtureMinutes := rand.Intn(4) + 12 // 12-15 分钟
	nurtureDuration := time.Duration(nurtureMinutes) * time.Minute
	targetLikes := rand.Intn(3)   // 0-2
	targetFollows := rand.Intn(2) // 0-1
	commentInterval := rand.Intn(3) + 3
	logger.Print(tag, fmt.Sprintf("养号目标: 时长 %d 分钟, 点赞 %d 次, 关注 %d 次, 每 %d 个帖子看评论",
		nurtureMinutes, targetLikes, targetFollows, commentInterval))

	// 定位视口中央帖子的 JS（返回帖子索引）
	findCenterTweetJs := `(function(){
		var articles = document.querySelectorAll('article[data-testid="tweet"]');
		if(articles.length === 0) return -1;
		var viewCenter = window.innerHeight / 2;
		var bestIdx = -1;
		var bestDist = Infinity;
		for(var i = 0; i < articles.length; i++){
			var rect = articles[i].getBoundingClientRect();
			// 只考虑部分在视口内的帖子
			if(rect.bottom < 0 || rect.top > window.innerHeight) continue;
			var center = rect.top + rect.height / 2;
			var dist = Math.abs(center - viewCenter);
			if(dist < bestDist){
				bestDist = dist;
				bestIdx = i;
			}
		}
		return bestIdx;
	})()`

	// 在指定帖子内点赞
	likeInTweetJs := `(function(){
		var articles = document.querySelectorAll('article[data-testid="tweet"]');
		var idx = %d;
		if(idx < 0 || idx >= articles.length) return false;
		var likeBtn = articles[idx].querySelector('[data-testid="like"]');
		if(!likeBtn){ likeBtn = articles[idx].querySelector('[data-testid="unlike"]'); if(likeBtn) return false; }
		if(!likeBtn) return false;
		likeBtn.click();
		return true;
	})()`

	// 在指定帖子内打开评论（点击日期链接进入详情页）
	openCommentInTweetJs := `(function(){
		var articles = document.querySelectorAll('article[data-testid="tweet"]');
		var idx = %d;
		if(idx < 0 || idx >= articles.length) return false;
		// 找到包含 /status/ 的链接（日期链接）
		var statusLink = articles[idx].querySelector('div[data-testid="User-Name"] a[href*="/status/"]');
		if(!statusLink) return false;
		statusLink.click();
		return true;
	})()`

	// 在指定帖子内获取用户链接
	getUserLinkInTweetJs := `(function(){
		var articles = document.querySelectorAll('article[data-testid="tweet"]');
		var idx = %d;
		if(idx < 0 || idx >= articles.length) return "";
		// 获取用户名链接（第一个 a 标签，不包含日期链接）
		var userNameDiv = articles[idx].querySelector('[data-testid="User-Name"]');
		if(!userNameDiv) return "";
		// 找到包含用户名的第一个链接（不是日期链接）
		var links = userNameDiv.querySelectorAll('a');
		for(var i = 0; i < links.length; i++){
			var href = links[i].getAttribute('href');
			// 日期链接包含 /status/，用户名链接不包含
			if(href && !href.includes('/status/')){
				return links[i].href;
			}
		}
		return "";
	})()`

	// 4. 循环浏览
	startTime := time.Now()
	postsWatched := 0
	likesDone := 0
	followsDone := 0
	commentsBrowsed := 0
	var lastError string

	// 预估总帖子数（根据时长和停留时间估算）
	avgWatchTime := 20 // 平均每个帖子停留20秒
	estimatedPosts := int(nurtureDuration.Seconds() / float64(avgWatchTime))

	for time.Since(startTime) < nurtureDuration {
		select {
		case <-ctx.Done():
			logger.Print(tag, "context 已取消，提前终止循环")
			lastError = "context 超时或取消"
			goto endLoop
		default:
		}

		postsWatched++
		remainingPosts := estimatedPosts - postsWatched
		if remainingPosts < 1 {
			remainingPosts = 1
		}

		logger.Print(tag, fmt.Sprintf("=== 开始浏览第 %d 个帖子 ===", postsWatched))

		// 滚动 300-500px
		scrollAmount := rand.Intn(201) + 300
		logger.Print(tag, fmt.Sprintf("滚动 %d px", scrollAmount))
		scrollJs := fmt.Sprintf("window.scrollBy(0, %d)", scrollAmount)
		if err := chromedp.Run(silentCtx, chromedp.Evaluate(scrollJs, nil)); err != nil {
			logger.Print(tag, "滚动失败: "+err.Error())
			lastError = "滚动失败: " + err.Error()
			goto endLoop
		}
		time.Sleep(2 * time.Second)

		// 停留 15-25 秒
		watchDuration := time.Duration(rand.Intn(11)+15) * time.Second
		remaining := nurtureDuration - time.Since(startTime)
		if watchDuration > remaining {
			watchDuration = remaining
		}
		if watchDuration <= 0 {
			goto endLoop
		}
		logger.Print(tag, fmt.Sprintf("停留 %.0f 秒", watchDuration.Seconds()))
		time.Sleep(watchDuration)

		// 定位当前视口中央的帖子
		logger.Print(tag, "定位当前帖子...")
		var tweetIdx int
		if err := chromedp.Run(silentCtx, chromedp.Evaluate(findCenterTweetJs, &tweetIdx)); err != nil {
			logger.Print(tag, "定位帖子失败: "+err.Error())
			continue
		}
		if tweetIdx < 0 {
			logger.Print(tag, "未找到可见帖子")
			continue
		}
		logger.Print(tag, fmt.Sprintf("找到帖子索引: %d", tweetIdx))

		// 点赞（随机分布）
		if likesDone < targetLikes {
			// 计算执行概率：剩余点赞数 / 剩余帖子数
			likeProbability := float64(targetLikes-likesDone) / float64(remainingPosts)
			if rand.Float64() < likeProbability {
				logger.Print(tag, "尝试点赞...")
				var liked bool
				js := fmt.Sprintf(likeInTweetJs, tweetIdx)
				if err := chromedp.Run(silentCtx, chromedp.Evaluate(js, &liked)); err != nil {
					logger.Print(tag, "点赞失败: "+err.Error())
				} else if liked {
					likesDone++
					logger.Print(tag, "点赞成功")
					time.Sleep(time.Duration(rand.Intn(3)+2) * time.Second)
				}
			}
		}

		// 每隔 3-5 个帖子看评论
		if postsWatched%commentInterval == 0 {
			logger.Print(tag, "准备查看评论...")
			var opened bool
			js := fmt.Sprintf(openCommentInTweetJs, tweetIdx)
			if err := chromedp.Run(silentCtx, chromedp.Evaluate(js, &opened)); err != nil {
				logger.Print(tag, "打开评论失败: "+err.Error())
			} else if opened {
				logger.Print(tag, "进入详情页，等待加载...")
				time.Sleep(5 * time.Second)
				logger.Print(tag, "下滑 800px 到评论区")
				_ = chromedp.Run(silentCtx, chromedp.Evaluate("window.scrollBy(0, 800)", nil))
				browseDuration := time.Duration(rand.Intn(6)+10) * time.Second
				logger.Print(tag, fmt.Sprintf("浏览评论 %.0f 秒", browseDuration.Seconds()))
				time.Sleep(browseDuration)
				logger.Print(tag, "点击返回按钮...")
				_ = chromedp.Run(silentCtx, chromedp.Click(`button[data-testid="app-bar-back"]`, chromedp.ByQuery))
				time.Sleep(3 * time.Second)
				commentsBrowsed++
				logger.Print(tag, "已返回上一页")
			}
		}

		// 关注（随机分布，不在最后30秒执行）
		if followsDone < targetFollows && time.Since(startTime) < nurtureDuration-30*time.Second {
			// 计算执行概率：剩余关注数 / 剩余帖子数
			followProbability := float64(targetFollows-followsDone) / float64(remainingPosts)
			if rand.Float64() < followProbability {
				logger.Print(tag, "准备关注用户...")
				var userURL string
				js := fmt.Sprintf(getUserLinkInTweetJs, tweetIdx)
				if err := chromedp.Run(silentCtx, chromedp.Evaluate(js, &userURL)); err != nil {
					logger.Print(tag, "获取用户链接失败: "+err.Error())
				} else if userURL != "" {
					logger.Print(tag, "找到用户链接: "+userURL)
					logger.Print(tag, "导航至用户主页...")
					if err := chromedp.Run(silentCtx, chromedp.Navigate(userURL)); err != nil {
						logger.Print(tag, "导航至用户主页失败: "+err.Error())
					} else {
						logger.Print(tag, "已进入用户主页，等待加载...")
						time.Sleep(5 * time.Second)
						followJs := `(function(){
					var followBtn = document.querySelector('button[aria-label^="Follow @"]');
					if(!followBtn) return false;
					followBtn.click();
					return true;
				})()`
						var followed bool
						logger.Print(tag, "查找关注按钮...")
						if err := chromedp.Run(silentCtx, chromedp.Evaluate(followJs, &followed)); err == nil && followed {
							followsDone++
							logger.Print(tag, "关注成功")
						} else {
							logger.Print(tag, "未找到关注按钮或已关注")
						}
						logger.Print(tag, "返回首页...")
						time.Sleep(3 * time.Second)
						_ = chromedp.Run(silentCtx, chromedp.Navigate("https://x.com/home"))
						time.Sleep(5 * time.Second)
					}
				} else {
					logger.Print(tag, "未找到用户链接")
				}
			}
		}
		logger.Print(tag, fmt.Sprintf("=== 第 %d 个帖子浏览完成 ===", postsWatched))
	}

endLoop:
	totalDuration := time.Since(startTime)
	report := fmt.Sprintf("总时长 %.0f 秒, 浏览帖子: %d, 点赞: %d, 评论: %d, 关注: %d", totalDuration.Seconds(), postsWatched, likesDone, commentsBrowsed, followsDone)
	logger.Print(tag, report)

	if lastError != "" {
		return nurture.NurtureResult{
			Status:           "error",
			ActionsPerformed: report,
			ErrorInfo:        lastError,
		}, nil
	}
	return nurture.NurtureResult{
		Status:           "completed",
		ActionsPerformed: report,
	}, nil
}
