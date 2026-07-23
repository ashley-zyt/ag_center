package facebook

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/nurture"

	"github.com/chromedp/chromedp"
)

type facebookActions struct {
	logger *logx.Logger
	ctx    context.Context
}

func (a *facebookActions) HomeURL() string {
	return "https://www.facebook.com/"
}

func (a *facebookActions) Tag() string {
	return "FB_NURTURE"
}

func (a *facebookActions) CheckLogin(ctx context.Context) (string, error) {
	var status string
	checkLoginJs := `(function(){
		var loginForm = document.getElementById('login_form');
		var feed = document.querySelector('[role="feed"]');
		if (feed) return 'logged_in';
		if (loginForm) return 'not_logged_in';
		return 'abnormal';
	})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(checkLoginJs, &status)); err != nil {
		return "", err
	}
	return status, nil
}

func (a *facebookActions) IsAd(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *facebookActions) LikePost(ctx context.Context) error {
	likeJs := `(function(){
		var likeBtn = document.querySelector('[aria-label="Like"]');
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

func (a *facebookActions) BrowseComments(ctx context.Context) error {
	openCommentsJs := `(function(){
		var commentBtn = document.querySelector('[aria-label="Comment"]');
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
		var closeBtn = document.querySelector('[aria-label="Close"]');
		if(closeBtn) closeBtn.click();
	})()`
	_ = chromedp.Run(ctx, chromedp.Evaluate(closeCommentsJs, nil))
	a.logger.Print(a.Tag(), "已关闭评论区")
	return nil
}

func (a *facebookActions) FollowUser(ctx context.Context) error {
	openProfileJs := `(function(){
		var userLink = document.querySelector('a[href*="/profile.php"]');
		if(!userLink) userLink = document.querySelector('a[href*="/groups/"]');
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
		var followBtn = document.querySelector('[aria-label="Follow"]');
		if(!followBtn) return false;
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

	_ = chromedp.Run(ctx, chromedp.Navigate("https://www.facebook.com/"))
	time.Sleep(3 * time.Second)
	a.logger.Print(a.Tag(), "已返回首页")
	return nil
}

func (a *facebookActions) NextPost(ctx context.Context) error {
	scrollAmount := rand.Intn(600) + 300
	scrollJs := fmt.Sprintf("window.scrollBy(0, %d)", scrollAmount)
	if err := chromedp.Run(ctx, chromedp.Evaluate(scrollJs, nil)); err != nil {
		return err
	}
	return nil
}

func (a *facebookActions) MaxWatchSeconds() int {
	return 0
}

func (a *facebookActions) RecoveryURL() string {
	return ""
}

func (a *facebookActions) CheckPageError(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *facebookActions) PreNurture(ctx context.Context) error {
	return nil
}

func NurtureAccount(ctx context.Context, logger *logx.Logger, req nurture.NurtureRequest) (nurture.NurtureResult, error) {
	actions := &facebookActions{
		logger: logger,
		ctx:    ctx,
	}
	return nurture.Run(ctx, logger, actions)
}
