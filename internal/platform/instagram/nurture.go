package instagram

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

type instagramActions struct {
	logger *logx.Logger
	ctx    context.Context
}

func (a *instagramActions) HomeURL() string {
	return "https://www.instagram.com/"
}

func (a *instagramActions) Tag() string {
	return "IG_NURTURE"
}

func (a *instagramActions) CheckLogin(ctx context.Context) (string, error) {
	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return "", err
	}
	// 只要URL以 https://www.instagram.com/reels/ 开头就算已登录
	if strings.HasPrefix(currentURL, "https://www.instagram.com/reels/") {
		return "logged_in", nil
	}
	// 如果被重定向到登录页面
	if strings.HasPrefix(currentURL, "https://www.instagram.com/accounts/") {
		return "not_logged_in", nil
	}
	return "abnormal", nil
}

func (a *instagramActions) IsAd(ctx context.Context) (bool, error) {
	return false, nil
}

func (a *instagramActions) LikePost(ctx context.Context) error {
	likeJs := `(function(){
		var likeBtn = document.querySelector('svg[aria-label="Like"]');
		if(!likeBtn) return false;
		likeBtn.click();
		return true;
	})()`
	var liked bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(likeJs, &liked)); err != nil {
		a.logger.Print(a.Tag(), "点赞操作异常: "+err.Error())
		return err
	}
	if liked {
		a.logger.Print(a.Tag(), "点赞成功")
	} else {
		a.logger.Print(a.Tag(), "未找到点赞按钮，需要刷新页面")
		return fmt.Errorf("未找到点赞按钮")
	}
	return nil
}

func (a *instagramActions) BrowseComments(ctx context.Context) error {
	// 点击评论按钮打开评论框（aria-expanded="false"表示未展开）
	openCommentsJs := `(function(){
		var commentBtn = document.querySelector('div[aria-expanded="false"] svg[aria-label="Comment"]');
		if(!commentBtn) return false;
		var parent = commentBtn.closest('div[aria-expanded]');
		if(!parent) return false;
		parent.click();
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

	// 检测评论框是否真正打开（div[role="dialog"]）
	checkDialogJs := `(function(){
		var dialog = document.querySelector('div[role="dialog"]');
		return dialog !== null;
	})()`
	var dialogExists bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(checkDialogJs, &dialogExists)); err != nil {
		return err
	}
	if !dialogExists {
		a.logger.Print(a.Tag(), "评论框未出现")
		return nil
	}

	browseDuration := time.Duration(rand.Intn(6)+10) * time.Second
	a.logger.Print(a.Tag(), fmt.Sprintf("浏览评论 %.0f 秒", browseDuration.Seconds()))
	time.Sleep(browseDuration)

	// 关闭评论框：再次点击评论按钮（现在aria-expanded="true"）
	closeCommentsJs := `(function(){
		var commentBtn = document.querySelector('div[aria-expanded="true"] svg[aria-label="Comment"]');
		if(!commentBtn) return false;
		var parent = commentBtn.closest('div[aria-expanded]');
		if(!parent) return false;
		parent.click();
		return true;
	})()`
	var closed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(closeCommentsJs, &closed)); err != nil {
		return err
	}
	if !closed {
		a.logger.Print(a.Tag(), "关闭评论框失败")
		return nil
	}

	// 等待评论框关闭（检测dialog消失）
	for i := 0; i < 10; i++ {
		time.Sleep(1 * time.Second)
		var stillExists bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(checkDialogJs, &stillExists)); err != nil {
			return err
		}
		if !stillExists {
			a.logger.Print(a.Tag(), "评论框已关闭")
			return nil
		}
	}
	a.logger.Print(a.Tag(), "评论框关闭超时")
	return nil
}

func (a *instagramActions) FollowUser(ctx context.Context) error {
	// 在当前页面直接点击关注按钮（div[role="button"] 包含 "Follow" 文本）
	followJs := `(function(){
		var followBtns = document.querySelectorAll('div[role="button"]');
		for(var i = 0; i < followBtns.length; i++) {
			var btn = followBtns[i];
			var span = btn.querySelector('span');
			if(span && span.innerText && span.innerText.trim() === 'Follow') {
				btn.click();
				return true;
			}
		}
		return false;
	})()`
	var followed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(followJs, &followed)); err != nil {
		a.logger.Print(a.Tag(), "关注操作异常: "+err.Error())
		return err
	}
	if followed {
		a.logger.Print(a.Tag(), "关注成功")
	} else {
		a.logger.Print(a.Tag(), "未找到关注按钮或已关注")
	}
	return nil
}

func (a *instagramActions) NextPost(ctx context.Context) error {
	nextBtnSelector := `div[aria-label="Reels navigation controls"] div[aria-label="Navigate to next Reel"]`
	if err := chromedp.Run(ctx, chromedp.Click(nextBtnSelector, chromedp.ByQuery)); err != nil {
		return err
	}
	return nil
}

func (a *instagramActions) MaxWatchSeconds() int {
	return 60
}

func (a *instagramActions) RecoveryURL() string {
	return "https://www.instagram.com/reels/"
}

func (a *instagramActions) CheckPageError(ctx context.Context) (bool, error) {
	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return false, err
	}
	// URL 为 https://www.instagram.com/reels/ 说明页面加载失败（没有 reel ID）
	if currentURL == "https://www.instagram.com/reels/" {
		return true, nil
	}
	// 检测页面是否包含 "Something went wrong" 错误文本
	errorJs := `(function(){
		var body = document.body;
		if(!body) return false;
		return body.innerText.indexOf('Something went wrong') !== -1;
	})()`
	var hasError bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(errorJs, &hasError)); err != nil {
		return false, err
	}
	return hasError, nil
}

func (a *instagramActions) PreNurture(ctx context.Context) error {
	// 点击包含 svg[aria-label="Reels"] 的 a 标签跳转到 Reels 页面
	reelsClickJs := `(function(){
		var svgs = document.querySelectorAll('svg[aria-label="Reels"]');
		for(var i = 0; i < svgs.length; i++) {
			var a = svgs[i].closest('a');
			if(a) { a.click(); return true; }
		}
		return false;
	})()`
	var clicked bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(reelsClickJs, &clicked)); err != nil {
		a.logger.Print(a.Tag(), "点击Reels按钮失败，尝试直接导航: "+err.Error())
		if err := chromedp.Run(ctx, chromedp.Navigate("https://www.instagram.com/reels/")); err != nil {
			return fmt.Errorf("导航到Reels页面失败: %w", err)
		}
	} else if !clicked {
		a.logger.Print(a.Tag(), "未找到Reels按钮，尝试直接导航")
		if err := chromedp.Run(ctx, chromedp.Navigate("https://www.instagram.com/reels/")); err != nil {
			return fmt.Errorf("导航到Reels页面失败: %w", err)
		}
	}
	a.logger.Print(a.Tag(), "已跳转到Reels页面")
	return nil
}

func NurtureAccount(ctx context.Context, logger *logx.Logger, req nurture.NurtureRequest) (nurture.NurtureResult, error) {
	actions := &instagramActions{
		logger: logger,
		ctx:    ctx,
	}
	return nurture.Run(ctx, logger, actions)
}
