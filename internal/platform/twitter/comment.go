package twitter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"minimax_pro/internal/chromedputil"
	"minimax_pro/internal/logx"

	"github.com/chromedp/chromedp"
)

// ─────────────────────────── 常量与选择器 ───────────────────────────
// 选择器提取自真实页面 dump(2026-10)：主贴页(post.txt)与评论弹窗(dialog.txt)。
// 全部为 data-testid 结构性定位，不依赖页面文字/aria-label 等本地化文本。
// 唯一性说明：
//   - 主贴页存在多个 article[data-testid="tweet"]（主贴 + 评论区）与多个 [data-testid="reply"]，
//     因此评论按钮必须级联限定在 primaryColumn 下的**主贴** article 内；
//   - 主贴页也有一个内联回复框 [data-testid="tweetTextarea_0"]，评论弹窗内也有一个，
//     因此填写/提交必须级联限定在 div[aria-labelledby="modal-header"][role="dialog"] 内。
const (
	// selMainTweet 主贴主体（primaryColumn 下的第一个 article）
	selMainTweet = `div[data-testid="primaryColumn"] article[data-testid="tweet"]`
	// selReplyDialog 评论弹窗容器
	selReplyDialog = `div[aria-labelledby="modal-header"][role="dialog"]`
	// selCommentTextarea 评论弹窗内的输入框（Draft.js contenteditable）
	selCommentTextarea = selReplyDialog + ` [data-testid="tweetTextarea_0"]`
	// selReplyButton 评论弹窗内的 Reply 提交按钮
	selReplyButton = selReplyDialog + ` button[data-testid="tweetButton"]`
)

// CommentResult 评论执行结果。
type CommentResult struct {
	Status    string `json:"status"` // completed / not_logged_in / failed
	ErrorInfo string `json:"error_info,omitempty"`
}

// CommentOnTweet 对指定推文发表评论（回复）。
//
// 流程：打开推文详情页 → 登录态检测 → 等待主贴出现 → 点击主贴评论按钮 →
// 等待评论弹窗 → 填写评论内容（Draft.js，SendKeys 逐字输入）→ 点击 Reply → 验证弹窗关闭。
func CommentOnTweet(ctx context.Context, logger *logx.Logger, targetURL, content string) (CommentResult, error) {
	tag := "TW_CMT"

	if strings.TrimSpace(targetURL) == "" {
		return CommentResult{Status: "failed", ErrorInfo: "target_url is required"}, errors.New("target_url is required")
	}
	if strings.TrimSpace(content) == "" {
		return CommentResult{Status: "failed", ErrorInfo: "comment content is required"}, errors.New("comment content is required")
	}

	// 1. 打开推文详情页
	logger.Print(tag, "打开推文链接: "+targetURL)
	if err := chromedputil.NavigateAndWaitBody(ctx, logger, targetURL, tag); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}
	time.Sleep(3 * time.Second)

	// 2. 登录态检测：未登录会被重定向到登录页
	var loc string
	_ = chromedp.Run(ctx, chromedp.Location(&loc))
	if strings.Contains(loc, "/login") {
		logger.Print(tag, "检测到账号未登录")
		return CommentResult{Status: "not_logged_in", ErrorInfo: "账号未登录"}, errors.New("账号未登录")
	}

	// 3. 等待主贴出现
	if err := waitCommentMainTweet(ctx, logger, tag); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}

	// 4. 点击主贴评论按钮，打开评论弹窗
	if err := clickCommentReplyButton(ctx, logger, tag); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}

	// 5. 等待评论弹窗出现
	if err := waitCommentDialog(ctx, logger, tag); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}

	// 6. 填写评论内容
	if err := fillCommentText(ctx, logger, tag, content); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}

	// 6.5 填写后等待 3 秒，让 Draft.js 状态稳定、Reply 按钮解除禁用
	time.Sleep(3 * time.Second)

	// 7. 点击 Reply 并验证弹窗关闭
	if err := clickCommentSubmit(ctx, logger, tag); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}

	// 7.5 点击后等待 10 秒，让评论真正发布
	time.Sleep(10 * time.Second)

	// 8. 强校验：评论内容真的出现在评论区
	if err := verifyCommentPosted(ctx, logger, tag, content); err != nil {
		return CommentResult{Status: "failed", ErrorInfo: err.Error()}, err
	}

	logger.Print(tag, "评论发送成功")
	return CommentResult{Status: "completed"}, nil
}

// waitCommentMainTweet 等待主贴主体出现。
func waitCommentMainTweet(ctx context.Context, logger *logx.Logger, tag string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := chromedp.Run(waitCtx, chromedp.WaitVisible(selMainTweet, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("主贴未出现: %v", err)
	}
	logger.Print(tag, "主贴已加载")
	return nil
}

// clickCommentReplyButton 点击主贴内的评论按钮，打开评论弹窗。
func clickCommentReplyButton(ctx context.Context, logger *logx.Logger, tag string) error {
	logger.Print(tag, "点击主贴评论按钮")
	js := `(function(){
		var main = document.querySelector('` + selMainTweet + `');
		if(!main) return false;
		var btn = main.querySelector('[data-testid="reply"]');
		if(!btn) return false;
		try{btn.scrollIntoView({block:"center"});}catch(e){}
		btn.click();
		return true;
	})()`
	var clicked bool
	clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := chromedp.Run(clickCtx, chromedp.Evaluate(js, &clicked)); err != nil {
		return fmt.Errorf("点击评论按钮失败: %v", err)
	}
	if !clicked {
		return errors.New("未找到主贴的评论按钮")
	}
	logger.Print(tag, "已点击评论按钮")
	return nil
}

// waitCommentDialog 等待评论弹窗出现。
func waitCommentDialog(ctx context.Context, logger *logx.Logger, tag string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(waitCtx, chromedp.WaitVisible(selReplyDialog, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("评论弹窗未出现: %v", err)
	}
	logger.Print(tag, "评论弹窗已打开")
	return nil
}

// fillCommentText 在评论弹窗内填写评论内容。
// 与发文 fillText 同源：SendKeys 逐字输入以触发 DraftJS 原生键盘事件，而非直接改 DOM。
func fillCommentText(ctx context.Context, logger *logx.Logger, tag, text string) error {
	logger.Print(tag, "填写评论内容")
	r := []rune(text)
	if len(r) > 280 {
		text = string(r[:280])
		logger.Print(tag, "评论内容超过280字符，已截断")
	}
	sel := selCommentTextarea

	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	err := chromedp.Run(waitCtx, chromedp.WaitVisible(sel, chromedp.ByQuery))
	cancelWait()
	if err != nil {
		return fmt.Errorf("评论输入框未出现: %v", err)
	}

	// 点击输入框获得焦点
	clickCtx, cancelClick := context.WithTimeout(ctx, 5*time.Second)
	_ = chromedp.Run(clickCtx, chromedp.Click(sel, chromedp.ByQuery))
	cancelClick()
	time.Sleep(300 * time.Millisecond)

	// SendKeys 逐字输入，触发 DraftJS 原生键盘事件
	typeCtx, cancelType := context.WithTimeout(ctx, 60*time.Second)
	defer cancelType()
	for _, ch := range text {
		if err := chromedp.Run(typeCtx, chromedp.SendKeys(sel, string(ch), chromedp.ByQuery)); err != nil {
			return fmt.Errorf("填写评论内容失败: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 验证内容已填入
	var confirmed bool
	confirmJs := `(function(){
		var el = document.querySelector('` + selCommentTextarea + `');
		if(!el) return false;
		var inner = el.querySelector('.public-DraftEditor-content');
		var content = inner ? (inner.textContent || '') : (el.textContent || '');
		return content.length > 0;
	})()`
	_ = chromedp.Run(ctx, chromedp.Evaluate(confirmJs, &confirmed))
	if !confirmed {
		return errors.New("评论内容未确认填写成功")
	}
	logger.Print(tag, "评论内容已填写")
	return nil
}

// clickCommentSubmit 等待 Reply 按钮可用后点击，并轮询验证弹窗关闭（评论发送成功）。
func clickCommentSubmit(ctx context.Context, logger *logx.Logger, tag string) error {
	logger.Print(tag, "点击 Reply 按钮")

	// 等待 Reply 按钮从禁用变为可用（填写文本后 React 会移除 disabled）
	readyJs := `(function(){
		var b = document.querySelector('` + selReplyButton + `');
		if(!b) return 'none';
		if(b.disabled || b.getAttribute('aria-disabled') === 'true') return 'disabled';
		return 'ready';
	})()`
	deadline := time.Now().Add(30 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		var state string
		stepCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_ = chromedp.Run(stepCtx, chromedp.Evaluate(readyJs, &state))
		cancel()
		if state == "ready" {
			ready = true
			break
		}
		if state == "none" {
			return errors.New("未找到 Reply 按钮")
		}
		time.Sleep(1 * time.Second)
	}
	if !ready {
		return errors.New("Reply 按钮未在超时内变为可用")
	}

	// 点击 Reply
	clickJs := `(function(){
		var b = document.querySelector('` + selReplyButton + `');
		if(!b) return false;
		try{b.click(); return true;}catch(e){return false;}
	})()`
	var clicked bool
	clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
	defer cancelClick()
	if err := chromedp.Run(clickCtx, chromedp.Evaluate(clickJs, &clicked)); err != nil {
		return fmt.Errorf("点击 Reply 失败: %v", err)
	}
	if !clicked {
		return errors.New("Reply 按钮不可点击")
	}
	logger.Print(tag, "已点击 Reply 按钮")

	// 验证弹窗关闭（评论发送成功的标志）
	goneJs := `(function(){
		return !document.querySelector('` + selReplyDialog + `');
	})()`
	goneDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(goneDeadline) {
		var gone bool
		checkCtx, cancelCheck := context.WithTimeout(ctx, 3*time.Second)
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(goneJs, &gone))
		cancelCheck()
		if gone {
			logger.Print(tag, "评论弹窗已关闭，发送成功")
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return errors.New("点击 Reply 后评论弹窗未关闭，无法确认发送成功")
}

// verifyCommentPosted 强校验：评论内容真的出现在推文评论区。
// 滚动加载评论区，匹配我方刚发的评论文本（前若干字符），避免「弹窗关闭但实际没发出去」的误判。
func verifyCommentPosted(ctx context.Context, logger *logx.Logger, tag, content string) error {
	needle := firstNChars(strings.TrimSpace(content), 8)
	if needle == "" {
		return errors.New("评论内容为空，无法校验")
	}
	logger.Print(tag, "校验评论是否已发布到评论区")

	js := fmt.Sprintf(`(function(){
		var needle = %q;
		var articles = document.querySelectorAll('article[data-testid="tweet"]');
		for(var i=0;i<articles.length;i++){
			var textEl = articles[i].querySelector('[data-testid="tweetText"]');
			if(!textEl) continue;
			var t = (textEl.textContent || '').trim();
			if(t.length > 0 && t.indexOf(needle) !== -1) return true;
		}
		return false;
	})()`, needle)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var found bool
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = chromedp.Run(checkCtx, chromedp.Evaluate(js, &found))
		cancel()
		if found {
			logger.Print(tag, "已确认评论出现在评论区")
			return nil
		}
		// 滚动加载更多评论
		_ = chromedp.Run(ctx, chromedp.Evaluate(`window.scrollTo(0, document.body.scrollHeight)`, nil))
		time.Sleep(2 * time.Second)
	}
	return errors.New("评论发送后未在评论区找到我方评论，无法确认成功")
}
