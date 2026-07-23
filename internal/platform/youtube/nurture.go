package youtube

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/nurture"

	"github.com/chromedp/chromedp"
)

type youtubeActions struct {
	logger *logx.Logger
	ctx    context.Context
}

func (a *youtubeActions) HomeURL() string {
	return "https://www.youtube.com/shorts/"
}

func (a *youtubeActions) Tag() string {
	return "YT_NURTURE"
}

func (a *youtubeActions) CheckLogin(ctx context.Context) (string, error) {
	var status string
	checkLoginJs := `(function(){
		var avatarBtn = document.querySelector('button#avatar-btn');
		var signInBtn = document.querySelector('a[href^="/signin"], a[href*="signin?"]');
		if (avatarBtn) return 'logged_in';
		if (signInBtn) return 'not_logged_in';
		return 'abnormal';
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(checkLoginJs, &status)); err != nil {
		return "", err
	}
	return status, nil
}

func (a *youtubeActions) IsAd(ctx context.Context) (bool, error) {
	isAdJs := `(function(){
		var adElement = document.querySelector('ad-button-view-model');
		return !!adElement;
	})()`
	var isAd bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(isAdJs, &isAd)); err != nil {
		return false, err
	}
	return isAd, nil
}

func (a *youtubeActions) LikePost(ctx context.Context) error {
	likeJs := `(function(){
		// 优先使用 like-button-view-model（旧版），否则通过心形 SVG path 特征匹配
		var likeBtn = document.querySelector('like-button-view-model button');
		if(likeBtn){ likeBtn.click(); return true; }
		var buttons = document.querySelectorAll('button[aria-pressed]');
		for(var i=0; i<buttons.length; i++){
			var path = buttons[i].querySelector('svg path');
			if(path && path.getAttribute('d') && path.getAttribute('d').indexOf('M16.25') === 0){
				buttons[i].click();
				return true;
			}
		}
		return false;
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

func (a *youtubeActions) BrowseComments(ctx context.Context) error {
	// 点击评论按钮打开评论框
	openCommentsJs := `(function(){
		// 通过对话气泡 SVG path 特征匹配评论按钮
		var buttons = document.querySelectorAll('button');
		for(var i=0; i<buttons.length; i++){
			var path = buttons[i].querySelector('svg path');
			if(path && path.getAttribute('d') && path.getAttribute('d').indexOf('M1 6a4 4 0 014-4h14') === 0){
				buttons[i].click();
				return true;
			}
		}
		return false;
	})()`
	var opened bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(openCommentsJs, &opened)); err != nil {
		return err
	}
	if !opened {
		a.logger.Print(a.Tag(), "未找到评论按钮")
		return nil
	}

	// 等待评论框出现：检查 visibility="ENGAGEMENT_PANEL_VISIBILITY_EXPANDED"
	waitOpenJs := `(function(){
		var panel = document.querySelector('div#shorts-panel-container ytd-engagement-panel-section-list-renderer[visibility="ENGAGEMENT_PANEL_VISIBILITY_EXPANDED"]');
		return !!panel;
	})()`
	var panelOpened bool
	for i := 0; i < 30; i++ {
		if err := chromedp.Run(ctx, chromedp.Evaluate(waitOpenJs, &panelOpened)); err != nil {
			return err
		}
		if panelOpened {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !panelOpened {
		a.logger.Print(a.Tag(), "评论框未出现")
		return nil
	}

	// 浏览评论 10-15 秒
	browseDuration := time.Duration(rand.Intn(6)+10) * time.Second
	time.Sleep(browseDuration)

	// 点击评论框内的关闭按钮
	closeCommentsJs := `(function(){
		// 通过 X 形 SVG path 特征匹配关闭按钮
		var panel = document.querySelector('ytd-engagement-panel-section-list-renderer');
		if(!panel) return false;
		var buttons = panel.querySelectorAll('button');
		for(var i=0; i<buttons.length; i++){
			var path = buttons[i].querySelector('svg path');
			if(path && path.getAttribute('d') && path.getAttribute('d').indexOf('M17.293 5.293 12 10.586') === 0){
				buttons[i].click();
				return true;
			}
		}
		return false;
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(closeCommentsJs, nil)); err != nil {
		return err
	}

	// 等待评论框关闭：检查 visibility="ENGAGEMENT_PANEL_VISIBILITY_HIDDEN"
	waitCloseJs := `(function(){
		var panel = document.querySelector('div#shorts-panel-container ytd-engagement-panel-section-list-renderer[visibility="ENGAGEMENT_PANEL_VISIBILITY_HIDDEN"]');
		return !!panel;
	})()`
	var panelClosed bool
	for i := 0; i < 30; i++ {
		if err := chromedp.Run(ctx, chromedp.Evaluate(waitCloseJs, &panelClosed)); err != nil {
			return err
		}
		if panelClosed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !panelClosed {
		a.logger.Print(a.Tag(), "评论框关闭超时")
	}
	time.Sleep(2 * time.Second)
	return nil
}

func (a *youtubeActions) FollowUser(ctx context.Context) error {
	// 获取频道链接
	openChannelJs := `(function(){
		var channelLink = document.querySelector('yt-reel-channel-bar-view-model a.ytAttributedStringLink');
		if(!channelLink) return "";
		return channelLink.href;
	})()`
	var channelURL string
	if err := chromedp.Run(ctx, chromedp.Evaluate(openChannelJs, &channelURL)); err != nil {
		return err
	}
	if channelURL == "" {
		a.logger.Print(a.Tag(), "未找到频道链接")
		return nil
	}

	if err := chromedp.Run(ctx, chromedp.Navigate(channelURL)); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)

	// 点击订阅按钮
	subscribeJs := `(function(){
		var subscribeBtn = document.querySelector('button[aria-label*="订阅"], button[aria-label*="Subscribe"]');
		if(!subscribeBtn) return false;
		if(subscribeBtn.innerText && (subscribeBtn.innerText.includes('已订阅') || subscribeBtn.innerText.includes('Subscribed'))) return false;
		subscribeBtn.click();
		return true;
	})()`
	var subscribed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(subscribeJs, &subscribed)); err != nil {
		return err
	}
	if subscribed {
		a.logger.Print(a.Tag(), "订阅成功")
	} else {
		a.logger.Print(a.Tag(), "未找到订阅按钮或已订阅")
	}
	time.Sleep(5 * time.Second)

	// 返回首页
	_ = chromedp.Run(ctx, chromedp.Navigate("https://www.youtube.com/shorts/"))
	time.Sleep(3 * time.Second)
	return nil
}

func (a *youtubeActions) NextPost(ctx context.Context) error {
	// 点击 Next video 按钮切换下一个短视频
	// 使用 #navigation-button-down 容器内的 button，避免 aria-label 语言依赖
	nextBtnSelector := `#navigation-button-down button`
	if err := chromedp.Run(ctx, chromedp.Click(nextBtnSelector, chromedp.ByQuery)); err != nil {
		a.logger.Print(a.Tag(), "切换视频失败: "+err.Error())
		return fmt.Errorf("没有找到下一个视频按钮")
	}
	return nil
}

func (a *youtubeActions) MaxWatchSeconds() int {
	return 60
}

func (a *youtubeActions) RecoveryURL() string {
	return "https://www.youtube.com/shorts/"
}

func (a *youtubeActions) CheckPageError(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *youtubeActions) PreNurture(ctx context.Context) error {
	return nil
}

func NurtureAccount(ctx context.Context, logger *logx.Logger, req nurture.NurtureRequest) (nurture.NurtureResult, error) {
	actions := &youtubeActions{
		logger: logger,
		ctx:    ctx,
	}
	return nurture.Run(ctx, logger, actions)
}
