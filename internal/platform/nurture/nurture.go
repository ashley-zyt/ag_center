package nurture

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"minimax_pro/internal/logx"

	"github.com/chromedp/chromedp"
)

type NurtureRequest struct {
	ProfileName      string `json:"profile_name"`
	Platform         string `json:"platform"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	WaitSeconds      int    `json:"wait_seconds"`
	UndetectablePath string `json:"undetectable_path"`
}

type NurtureResult struct {
	Status           string `json:"status"`
	ActionsPerformed string `json:"actions_performed,omitempty"`
	ErrorInfo        string `json:"error_info,omitempty"`
}

// PlatformActions 各平台需实现的养号操作接口
type PlatformActions interface {
	// HomeURL 返回首页地址
	HomeURL() string
	// Tag 返回平台日志标签，如 "TT_NURTURE"
	Tag() string
	// CheckLogin 检查登录状态，返回状态字符串：
	// "logged_in" - 已登录
	// "not_logged_in" - 未登录
	// "abnormal" - 状态异常，需人工检查
	CheckLogin(ctx context.Context) (string, error)
	// IsAd 检测当前页面是否为广告
	IsAd(ctx context.Context) (bool, error)
	// LikePost 点赞当前帖子/视频
	LikePost(ctx context.Context) error
	// BrowseComments 打开并浏览评论区（含打开、滚动、关闭）
	BrowseComments(ctx context.Context) error
	// FollowUser 进入作者主页并关注，需自行处理返回首页
	FollowUser(ctx context.Context) error
	// NextPost 切换到下一个帖子/视频
	NextPost(ctx context.Context) error
	// MaxWatchSeconds 返回单个视频最大停留秒数，0 表示不限制
	MaxWatchSeconds() int
	// RecoveryURL 返回恢复页面URL（切换失败或超时时重新加载），空字符串表示使用 HomeURL
	RecoveryURL() string
	// PreNurture 导航到首页后的额外操作（如 Instagram 需要点击 Reels 按钮跳转），默认无操作
	PreNurture(ctx context.Context) error
	// CheckPageError 检测当前页面是否出现错误（如 Instagram 的 "Something went wrong"），返回 true 表示页面异常需要重新加载
	CheckPageError(ctx context.Context) (bool, error)
}

// Run 统一养号流程控制
func Run(ctx context.Context, logger *logx.Logger, p PlatformActions) (NurtureResult, error) {
	tag := p.Tag()
	silentCtx, silentCancel := chromedp.NewContext(ctx,
		chromedp.WithErrorf(func(string, ...interface{}) {}),
	)
	defer silentCancel()

	// 1. 打开首页
	homeURL := p.HomeURL()
	logger.Print(tag, "正在导航至首页: "+homeURL)
	if err := chromedp.Run(silentCtx, chromedp.Navigate(homeURL)); err != nil {
		return NurtureResult{Status: "failed", ErrorInfo: "导航失败: " + err.Error()}, fmt.Errorf("navigate failed: %w", err)
	}
	time.Sleep(8 * time.Second)

	// 1.5 导航后额外操作（如 Instagram 点击 Reels 按钮跳转）
	if err := p.PreNurture(silentCtx); err != nil {
		logger.Print(tag, "导航后额外操作失败: "+err.Error())
	}
	time.Sleep(3 * time.Second)

	// 2. 检查登录状态
	loginStatus, err := p.CheckLogin(silentCtx)
	if err != nil {
		return NurtureResult{Status: "failed", ErrorInfo: "检查登录失败: " + err.Error()}, fmt.Errorf("check login failed: %w", err)
	}
	if loginStatus == "not_logged_in" {
		logger.Print(tag, "检测到账号未登录")
		return NurtureResult{Status: "not_logged_in", ErrorInfo: "账号未登录"}, fmt.Errorf("账号未登录")
	}
	if loginStatus == "abnormal" {
		logger.Print(tag, "账号状态异常，需人工检查")
		return NurtureResult{Status: "abnormal", ErrorInfo: "账号状态异常，需人工检查"}, fmt.Errorf("账号状态异常")
	}
	logger.Print(tag, "登录状态检查通过")

	// 3. 随机生成养号目标
	nurtureMinutes := rand.Intn(2) + 12 // 12-15 分钟
	// nurtureMinutes := rand.Intn(4) + 2 // 12-15 分钟
	nurtureDuration := time.Duration(nurtureMinutes) * time.Minute
	targetLikes := rand.Intn(3)         // 0-2
	targetFollows := rand.Intn(2)       // 0-1
	commentInterval := rand.Intn(3) + 3 // 每 3-5 个帖子看评论

	logger.Print(tag, fmt.Sprintf("养号目标: 时长 %d 分钟, 点赞 %d 次, 关注 %d 次, 每 %d 个帖子看评论",
		nurtureMinutes, targetLikes, targetFollows, commentInterval))

	// 4. 循环浏览
	startTime := time.Now()
	postsWatched := 0
	likesDone := 0
	followsDone := 0
	commentsBrowsed := 0
	var lastError string

	// 预估总帖子数（根据时长和平均停留时间估算）
	avgWatchTime := 20 // 平均每个帖子停留20秒
	estimatedPosts := int(nurtureDuration.Seconds() / float64(avgWatchTime))

	maxWatchSeconds := p.MaxWatchSeconds()
	recoveryURL := p.RecoveryURL()
	if recoveryURL == "" {
		recoveryURL = p.HomeURL()
	}

	// 硬性时间上限：15分钟
	maxTotalDuration := 15 * time.Minute

	logger.Print(tag, fmt.Sprintf("预估帖子数: %d, 最大停留: %d秒, 恢复URL: %s", estimatedPosts, maxWatchSeconds, recoveryURL))
	logger.Print(tag, "进入主循环...")
	for time.Since(startTime) < nurtureDuration {
		// 检查是否超过硬性时间上限
		if time.Since(startTime) >= maxTotalDuration {
			logger.Print(tag, fmt.Sprintf("已达到硬性时间上限 %d 分钟，强制结束", int(maxTotalDuration.Minutes())))
			goto endLoop
		}

		// 检查 context 是否已取消
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

		// 检查 context 状态
		if silentCtx.Err() != nil {
			logger.Print(tag, "silentCtx 已失效: "+silentCtx.Err().Error())
			goto endLoop
		}

		// 检测是否为广告
		logger.Print(tag, "检测广告...")
		isAd, err := p.IsAd(silentCtx)
		if err != nil {
			logger.Print(tag, "广告检测失败: "+err.Error())
			// context 失效时直接退出
			if silentCtx.Err() != nil {
				logger.Print(tag, "context 已失效，退出循环")
				goto endLoop
			}
		} else if isAd {
			logger.Print(tag, "检测到广告，跳过当前视频")
			// 切换下一个帖子
			if time.Since(startTime) < nurtureDuration {
				if err := p.NextPost(silentCtx); err != nil {
					logger.Print(tag, "切换失败: "+err.Error())
					lastError = "切换帖子失败: " + err.Error()
					goto endLoop
				}
				time.Sleep(time.Duration(rand.Intn(2)+1) * time.Second)
			}
			continue
		}

		// 浏览当前帖子 15-25 秒
		watchDuration := time.Duration(rand.Intn(11)+15) * time.Second
		remaining := nurtureDuration - time.Since(startTime)
		if watchDuration > remaining {
			watchDuration = remaining
		}
		if watchDuration <= 0 {
			goto endLoop
		}
		logger.Print(tag, fmt.Sprintf("浏览视频 %d 秒", int(watchDuration.Seconds())))
		time.Sleep(watchDuration)

		// 检查 context 状态
		if silentCtx.Err() != nil {
			logger.Print(tag, "浏览后 silentCtx 已失效: "+silentCtx.Err().Error())
			goto endLoop
		}

		// 检查是否超过最大停留时间
		if maxWatchSeconds > 0 && watchDuration > time.Duration(maxWatchSeconds)*time.Second {
			logger.Print(tag, fmt.Sprintf("视频停留超过 %d 秒，重新加载页面", maxWatchSeconds))
			if err := chromedp.Run(silentCtx, chromedp.Navigate(recoveryURL)); err != nil {
				logger.Print(tag, "重新加载失败: "+err.Error())
				lastError = "视频停留超时且恢复失败"
				goto endLoop
			}
			time.Sleep(3 * time.Second)
			continue
		}

		// 检测页面错误（如 Instagram 的 "Something went wrong"）
		if pageError, err := p.CheckPageError(silentCtx); err == nil && pageError {
			logger.Print(tag, "检测到页面错误，重新加载页面")
			if err := chromedp.Run(silentCtx, chromedp.Navigate(recoveryURL)); err != nil {
				logger.Print(tag, "重新加载失败: "+err.Error())
				lastError = "页面错误且恢复失败"
				goto endLoop
			}
			time.Sleep(3 * time.Second)
			continue
		}

		// 点赞（随机分布）
		if likesDone < targetLikes {
			// 计算执行概率：剩余点赞数 / 剩余帖子数
			likeProbability := float64(targetLikes-likesDone) / float64(remainingPosts)
			if rand.Float64() < likeProbability {
				if err := p.LikePost(silentCtx); err != nil {
					logger.Print(tag, "点赞失败，刷新页面恢复: "+err.Error())
					lastError = "点赞失败: " + err.Error()
					if errors.Is(err, context.Canceled) {
						goto endLoop
					}
					// 刷新页面恢复
					if err := chromedp.Run(silentCtx, chromedp.Navigate(recoveryURL)); err != nil {
						logger.Print(tag, "刷新页面失败: "+err.Error())
						lastError = "点赞失败且恢复失败"
						goto endLoop
					}
					time.Sleep(5 * time.Second)
					continue
				} else {
					likesDone++
					time.Sleep(time.Duration(rand.Intn(3)+2) * time.Second)
				}
			}
		}

		// 每隔 3-5 个帖子看评论
		if postsWatched%commentInterval == 0 {
			if err := p.BrowseComments(silentCtx); err != nil {
				logger.Print(tag, "浏览评论异常，刷新页面恢复: "+err.Error())
				lastError = "浏览评论失败: " + err.Error()
				if errors.Is(err, context.Canceled) {
					goto endLoop
				}
				// 刷新页面恢复
				if err := chromedp.Run(silentCtx, chromedp.Navigate(recoveryURL)); err != nil {
					logger.Print(tag, "刷新页面失败: "+err.Error())
					lastError = "评论失败且恢复失败"
					goto endLoop
				}
				time.Sleep(5 * time.Second)
				continue
			} else {
				commentsBrowsed++
			}
		}

		// 关注（随机分布，检查剩余时间是否足够进入主页并返回）
		if followsDone < targetFollows && time.Since(startTime) < nurtureDuration-30*time.Second {
			// 计算执行概率：剩余关注数 / 剩余帖子数
			followProbability := float64(targetFollows-followsDone) / float64(remainingPosts)
			if rand.Float64() < followProbability {
				if err := p.FollowUser(silentCtx); err != nil {
					logger.Print(tag, "关注异常，刷新页面恢复: "+err.Error())
					lastError = "关注失败: " + err.Error()
					if errors.Is(err, context.Canceled) {
						goto endLoop
					}
					// 刷新页面恢复
					if err := chromedp.Run(silentCtx, chromedp.Navigate(recoveryURL)); err != nil {
						logger.Print(tag, "刷新页面失败: "+err.Error())
						lastError = "关注失败且恢复失败"
						goto endLoop
					}
					time.Sleep(5 * time.Second)
					continue
				} else {
					followsDone++
				}
			}
		}

		// 切换下一个帖子
		if time.Since(startTime) < nurtureDuration {
			// 检查 context 状态
			if silentCtx.Err() != nil {
				logger.Print(tag, "切换前 silentCtx 已失效: "+silentCtx.Err().Error())
				goto endLoop
			}
			logger.Print(tag, "准备切换下一个视频...")
			if err := p.NextPost(silentCtx); err != nil {
				logger.Print(tag, "切换失败，重新加载页面: "+err.Error())
				// 切换失败时重新加载恢复页面
				if err := chromedp.Run(silentCtx, chromedp.Navigate(recoveryURL)); err != nil {
					logger.Print(tag, "重新加载失败: "+err.Error())
					lastError = "切换失败且重新加载失败: " + err.Error()
					goto endLoop
				}
				time.Sleep(3 * time.Second)
			} else {
				logger.Print(tag, "切换成功")
				time.Sleep(time.Duration(rand.Intn(2)+1) * time.Second)
			}
		}
	}

endLoop:

	totalDuration := time.Since(startTime)
	report := fmt.Sprintf("总时长 %.0f 秒, 浏览帖子: %d, 点赞: %d, 评论: %d, 关注: %d", totalDuration.Seconds(), postsWatched, likesDone, commentsBrowsed, followsDone)
	logger.Print(tag, report)

	// 如果有错误，返回错误信息和已完成的操作
	if lastError != "" {
		return NurtureResult{
			Status:           "error",
			ActionsPerformed: report,
			ErrorInfo:        lastError,
		}, nil
	}

	return NurtureResult{
		Status:           "completed",
		ActionsPerformed: report,
	}, nil
}
