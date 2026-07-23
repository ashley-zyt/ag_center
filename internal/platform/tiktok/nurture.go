package tiktok

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/nurture"

	"github.com/chromedp/chromedp"
)

type tiktokActions struct {
	logger *logx.Logger
	ctx    context.Context
}

func (a *tiktokActions) HomeURL() string {
	return "https://www.tiktok.com/"
}

func (a *tiktokActions) Tag() string {
	return "TT_NURTURE"
}

func (a *tiktokActions) CheckLogin(ctx context.Context) (string, error) {
	var status string
	checkLoginJs := `(function(){
		var selectors = [
			'button[id="top-right-action-bar-login-button"]>div[class="TUXButton-content"]>div[class="TUXButton-label"]',
			'button[id="header-login-button"]>div[class="TUXButton-content"]>div[class="TUXButton-label"]'
		];
		for (var i = 0; i < selectors.length; i++) {
			var el = document.querySelector(selectors[i]);
			if (el && el.innerText.trim() === 'Log in') {
				return 'not_logged_in';
			}
		}
		return 'logged_in';
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(checkLoginJs, &status)); err != nil {
		return "", err
	}
	return status, nil
}

func (a *tiktokActions) IsAd(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *tiktokActions) LikePost(ctx context.Context) error {
	if err := chromedp.Run(ctx, chromedp.Click(`div[data-e2e="like-icon"] button[data-testid="tux-web-icon-button"]`, chromedp.ByQuery)); err != nil {
		a.logger.Print(a.Tag(), "点赞失败: "+err.Error())
		return err
	}
	a.logger.Print(a.Tag(), "点赞成功")
	return nil
}

func (a *tiktokActions) BrowseComments(ctx context.Context) error {
	// 点击评论按钮打开评论框
	if err := chromedp.Run(ctx, chromedp.Click(`div[data-e2e="comment-icon"] button[data-testid="tux-web-icon-button"]`, chromedp.ByQuery)); err != nil {
		a.logger.Print(a.Tag(), "打开评论失败: "+err.Error())
		return err
	}
	a.logger.Print(a.Tag(), "已点击评论按钮，等待评论框出现")

	// 等待评论框出现
	commentSidebarSelector := `div[class*="comment-sidebar-transition-enter-done"]>section`
	if err := chromedp.Run(ctx, chromedp.WaitVisible(commentSidebarSelector, chromedp.ByQuery)); err != nil {
		a.logger.Print(a.Tag(), "等待评论框出现失败: "+err.Error())
		return err
	}
	a.logger.Print(a.Tag(), "评论框已出现")

	// 浏览评论 10-15 秒
	browseDuration := time.Duration(rand.Intn(6)+10) * time.Second
	a.logger.Print(a.Tag(), fmt.Sprintf("浏览评论 %.0f 秒", browseDuration.Seconds()))
	time.Sleep(browseDuration)

	// 再次点击评论按钮关闭评论框
	if err := chromedp.Run(ctx, chromedp.Click(`div[data-e2e="comment-icon"] button[data-testid="tux-web-icon-button"]`, chromedp.ByQuery)); err != nil {
		a.logger.Print(a.Tag(), "关闭评论失败: "+err.Error())
		return err
	}
	a.logger.Print(a.Tag(), "已点击评论按钮关闭，等待评论框消失")

	// 等待评论框消失（检查元素是否从DOM中移除）
	waitJs := `(function(){
		var el = document.querySelector('div[class*="comment-sidebar-transition-enter-done"]>section');
		return !el;
	})()`
	var removed bool
	for i := 0; i < 30; i++ { // 最多等待3秒
		if err := chromedp.Run(ctx, chromedp.Evaluate(waitJs, &removed)); err != nil {
			return err
		}
		if removed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.logger.Print(a.Tag(), "评论框已关闭")
	time.Sleep(2 * time.Second)
	return nil
}

func (a *tiktokActions) FollowUser(ctx context.Context) error {
	var href string
	if err := chromedp.Run(ctx, chromedp.AttributeValue(`a[data-e2e="video-author-avatar"]`, "href", &href, nil)); err != nil {
		a.logger.Print(a.Tag(), "获取作者链接失败: "+err.Error())
		return err
	}
	if href == "" {
		a.logger.Print(a.Tag(), "未找到作者链接")
		return nil
	}
	profileURL := href
	if !strings.HasPrefix(href, "http") {
		profileURL = "https://www.tiktok.com" + href
	}
	a.logger.Print(a.Tag(), "进入作者主页: "+profileURL)

	if err := chromedp.Run(ctx, chromedp.Navigate(profileURL)); err != nil {
		a.logger.Print(a.Tag(), "导航至作者主页失败: "+err.Error())
		return err
	}
	time.Sleep(5 * time.Second)

	followJs := `(function(){
		var followBtn = document.querySelector('button[data-e2e="follow-button"]');
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

	time.Sleep(3 * time.Second)

	// 返回首页
	if err := chromedp.Run(ctx, chromedp.Navigate("https://www.tiktok.com/")); err != nil {
		a.logger.Print(a.Tag(), "返回首页失败: "+err.Error())
		return err
	}
	time.Sleep(5 * time.Second)
	a.logger.Print(a.Tag(), "已返回首页")
	return nil
}

func (a *tiktokActions) NextPost(ctx context.Context) error {
	if err := chromedp.Run(ctx, chromedp.Click(`div[class*="DivFeedNavigationContainer"] > div:nth-of-type(2) > button`, chromedp.ByQuery)); err != nil {
		a.logger.Print(a.Tag(), "点击向下翻页失败: "+err.Error())
		return err
	}
	return nil
}

func (a *tiktokActions) MaxWatchSeconds() int {
	return 0
}

func (a *tiktokActions) RecoveryURL() string {
	return ""
}

func (a *tiktokActions) CheckPageError(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *tiktokActions) PreNurture(ctx context.Context) error {
	return nil
}

// NurtureAccount 实现养号功能（供 main.go 调度器调用）
func NurtureAccount(ctx context.Context, logger *logx.Logger, req nurture.NurtureRequest) (nurture.NurtureResult, error) {
	actions := &tiktokActions{
		logger: logger,
		ctx:    ctx,
	}
	return nurture.Run(ctx, logger, actions)
}
