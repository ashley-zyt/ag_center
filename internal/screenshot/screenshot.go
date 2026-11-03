package screenshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"minimax_pro/internal/logx"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// 报错截图：任务执行报错时，把浏览器当前页面的视口截图上传到 OSS 的 ag-error bucket，
// 并把可访问 URL 回传给 account_sys（供其单独处理/排查）。
//
// 设计说明（第二层全流程通用拦截）：
//   - 截图必须在浏览器 tab 上下文还存活时完成（停止 profile / 关闭标签页之后 CDP 已断开，
//     chromedp 会整体 Cancel，此时截图必然失败），因此不能放在 finalizeAsyncTask 收尾处，
//     而要在各执行链的"报错点、浏览器还活着"的地方触发；
//   - URL 通过 context 里注入的回调（WithShotSink）上报到任务名下，再由 finalizeAsyncTask
//     统一塞进回调 payload。

const (
	// errorBucket 报错截图存储的 OSS bucket。
	errorBucket = "ag-error"
	// uploadSignatureURL account_sys 的 OSS 上传凭证接口（与视频上传同一入口，无 HMAC 鉴权）。
	uploadSignatureURL = "https://agweb.juzhiic.com/api/v1/oss/upload_signature"
	// ossUserAgent 与 account_sys 交互时使用的 UA。
	ossUserAgent = "Apifox/1.0.0 (https://apifox.com)"
)

// Capture 截取当前页面的视口截图（PNG 字节）。
// ctx 必须是存活中的 chromedp 标签页/浏览器上下文。
func Capture(ctx context.Context) ([]byte, error) {
	// 截图必须带独立超时：调用方传入的 tabCtx 往往没有 deadline，若页面正处在
	// 重定向/加载中，CDP 命令可能一直不返回，导致任务无限阻塞。
	shotCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 用 window.stop() 停掉当前导航（runtime 域命令，能快速返回），让 renderer 空闲。
	_ = chromedp.Run(shotCtx, chromedp.Evaluate(`window.stop()`, nil))
	time.Sleep(300 * time.Millisecond)

	// 用底层 page.CaptureScreenshot（不设 fromSurface，即 fromSurface=false 走 renderer 渲染截图）。
	// 经 chromedp.Run 包装以注入 target executor。
	var buf []byte
	err := chromedp.Run(shotCtx, chromedp.ActionFunc(func(actCtx context.Context) error {
		var err error
		buf, err = page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			Do(actCtx)
		return err
	}))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// ossSignature account_sys /api/v1/oss/upload_signature 返回的直传凭证。
type ossSignature struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Endpoint    string `json:"endpoint"`
	AccessKeyID string `json:"accessKeyId"`
	Policy      string `json:"policy"`
	Signature   string `json:"signature"`
	URL         string `json:"url"`
}

type ossSignatureResp struct {
	Type string       `json:"type"`
	Data ossSignature `json:"data"`
}

// UploadErrorScreenshot 把 PNG 上传到 OSS 的 ag-error bucket，返回可访问 URL。
// 内部使用独立的 Background 超时上下文做 HTTP 请求，避免受浏览器上下文的取消影响。
func UploadErrorScreenshot(logger *logx.Logger, png []byte, filename string) (string, error) {
	sig, err := fetchUploadSignature(filename)
	if err != nil {
		return "", err
	}
	if err := uploadToOSS(sig, png); err != nil {
		return "", err
	}
	return sig.URL, nil
}

// fetchUploadSignature 向 account_sys 拿 OSS 直传凭证（bucket=ag-error）。
func fetchUploadSignature(filename string) (ossSignature, error) {
	// account_sys 只从 filename 提取扩展名生成 OSS key，必须保证带 .png，否则 key 无扩展名。
	filename = strings.TrimSpace(filename)
	if filename == "" || !strings.HasSuffix(strings.ToLower(filename), ".png") {
		filename = "error.png"
	}
	// 该接口只接受 form 编码参数（application/x-www-form-urlencoded）；
	// JSON body 传参会返回 400（实测 2026-10-29）。
	form := url.Values{}
	form.Set("filename", filename)
	form.Set("bucket", errorBucket)

	reqCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, uploadSignatureURL, strings.NewReader(form.Encode()))
	if err != nil {
		return ossSignature{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ossUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ossSignature{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed ossSignatureResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ossSignature{}, fmt.Errorf("解析上传凭证失败: %v body=%s", err, string(raw))
	}
	sig := parsed.Data
	if sig.Endpoint == "" || sig.AccessKeyID == "" || sig.Policy == "" || sig.Signature == "" || sig.Key == "" {
		return ossSignature{}, fmt.Errorf("上传凭证字段不完整: %s", string(raw))
	}
	return sig, nil
}

// uploadToOSS 按阿里云 OSS 直传协议把 PNG multipart 上传（file 字段放最后）。
func uploadToOSS(sig ossSignature, png []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("key", sig.Key); err != nil {
		return err
	}
	if err := w.WriteField("OSSAccessKeyId", sig.AccessKeyID); err != nil {
		return err
	}
	if err := w.WriteField("policy", sig.Policy); err != nil {
		return err
	}
	if err := w.WriteField("signature", sig.Signature); err != nil {
		return err
	}
	fw, err := w.CreateFormFile("file", "error.png")
	if err != nil {
		return err
	}
	if _, err := fw.Write(png); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, sig.Endpoint, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Errorf("OSS 直传失败: status=%d body=%s", resp.StatusCode, string(raw))
}

// ─────────────────────────── 报错截图上报回调 ───────────────────────────

type shotSinkKey struct{}

// WithShotSink 往 ctx 注入报错截图回调：截图上传成功后调用 onShot(url)。
// 由 cmd/server 在 registerTask 时注入，把 URL 记到对应 taskID 名下。
func WithShotSink(ctx context.Context, onShot func(string)) context.Context {
	return context.WithValue(ctx, shotSinkKey{}, onShot)
}

func shotSink(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	if fn, ok := ctx.Value(shotSinkKey{}).(func(string)); ok {
		return fn
	}
	return nil
}

// OnErrorDefer 供 `defer` 使用：若 err 非 nil，则从 shotCtx 截图并上传，通过 sinkCtx 回调上报。
//
// shotCtx 必须是存活中的 chromedp 标签页/浏览器上下文；sinkCtx 是携带 WithShotSink 回调的
// 上下文（通常是任务根 context）。两者分离是为了不依赖 chromedp 上下文的传值行为。
// websocketURL 用于 CDP 截图失败时做系统级窗口截屏的窗口定位。
func OnErrorDefer(shotCtx, sinkCtx context.Context, logger *logx.Logger, filename, websocketURL string, err *error) {
	if err == nil || *err == nil {
		return
	}
	captureAndUpload(shotCtx, sinkCtx, logger, filename, websocketURL)
}

// CaptureIfError 若 failed 为 true，则从 shotCtx 截图并上传，通过 sinkCtx 回调上报。
// 用于返回值是状态元组（而非单个 error）的执行函数，在浏览器还活着时显式调用。
func CaptureIfError(shotCtx, sinkCtx context.Context, logger *logx.Logger, filename, websocketURL string, failed bool) {
	if !failed {
		return
	}
	captureAndUpload(shotCtx, sinkCtx, logger, filename, websocketURL)
}

func captureAndUpload(shotCtx, sinkCtx context.Context, logger *logx.Logger, filename, websocketURL string) {
	if shotCtx == nil || shotCtx.Err() != nil {
		logger.Print("SHOT", "报错截图跳过：浏览器上下文已失效")
		return
	}
	png, err := Capture(shotCtx)
	if err != nil {
		logger.Print("SHOT", "CDP 截图失败，改用系统级窗口截屏: "+err.Error())
		png, err = CaptureWindow(websocketURL)
		if err != nil {
			logger.Print("SHOT", "系统级窗口截屏失败: "+err.Error())
			return
		}
	}
	url, err := UploadErrorScreenshot(logger, png, filename)
	if err != nil {
		logger.Print("SHOT", "报错截图上传失败: "+err.Error())
		return
	}
	logger.Print("SHOT", "报错截图已上传: "+url)
	if fn := shotSink(sinkCtx); fn != nil {
		fn(url)
	}
}
