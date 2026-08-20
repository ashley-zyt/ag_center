// Package message 提供跨平台私信模块的共享类型、平台操作接口与统一执行流程。
// 定位与 nurture 包一致: 框架负责通用流程(导航收件箱/登录检测/任务循环/随机间隔/结果汇总),
// 各平台只需实现 MessengerActions 的原子操作(选择器与DOM解析细节在平台包内维护)。
// 浏览器生命周期(Profile 启动/CDP连接/标签页清理/停止Profile)由 cmd/server 的 HTTP handler 负责,
// 框架拿到的 ctx 已是可直接执行 chromedp 操作的标签页上下文。
package message

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/platform/scraper"

	"github.com/chromedp/chromedp"
)

// Message 单条私信
type Message struct {
	SenderName string `json:"sender_name"`       // 发送者显示名(自己或对方)
	Direction  string `json:"direction"`         // outgoing=自己发出 / incoming=对方发来
	Content    string `json:"content"`           // 消息文本
	SentAt     string `json:"sent_at,omitempty"` // 页面展示时间原文(平台格式各异,不做转换)
	ObservedAt string `json:"observed_at,omitempty"` // 本次查询观察到该消息的服务端时间(RFC3339, 仅check_reply填充)
}

// Conversation 会话(对方+最新消息)
type Conversation struct {
	ConversationID string    `json:"conversation_id"`           // 平台会话标识(优先存可定位的链接/ID,回发消息时用于重新打开)
	PartnerName    string    `json:"partner_name"`              // 对方账号名
	PartnerURL     string    `json:"partner_url,omitempty"`     // 对方主页链接(若可从会话项解析)
	LastMessage    string    `json:"last_message,omitempty"`    // 会话列表中的最新消息预览
	LastMessageAt  string    `json:"last_message_at,omitempty"` // 列表展示时间原文
	Unread         bool      `json:"unread,omitempty"`          // 是否有未读标记
	Messages       []Message `json:"messages,omitempty"`        // 打开会话后填充的最新消息(时间正序: 旧->新)
}

// SendTask 单条主动发送任务
type SendTask struct {
	TargetURL      string `json:"target_url"`             // 对方账号主页链接(主页型平台: TikTok/Instagram/Facebook)
	AccountName    string `json:"account_name,omitempty"` // 对方账号名(搜索型平台: X/Twitter, 如 @Widino)
	MessageContent string `json:"message_content"`        // 消息内容
	Passcode       string `json:"passcode,omitempty"`     // 平台密码验证(如 X 私信 Passcode)
}

// SendOutcome 单条发送任务结果
type SendOutcome struct {
	TargetURL   string `json:"target_url,omitempty"`
	AccountName string `json:"account_name,omitempty"`
	Status      string `json:"status"` // sent / failed / not_logged_in
	ErrorInfo   string `json:"error_info,omitempty"`
}

// SendResult 一次批量发送的整体结果
type SendResult struct {
	Status    string        `json:"status"` // completed / partial_failed / failed / not_logged_in / error
	Results   []SendOutcome `json:"results"`
	ErrorInfo string        `json:"error_info,omitempty"`
}

// FetchOptions 会话拉取选项
type FetchOptions struct {
	MaxConversations           int    // 最多处理的会话数, 0=默认10
	MaxMessagesPerConversation int    // 每个会话最多保留的最新消息数, 0=默认20
	Passcode                   string // 平台密码验证(如 X 私信 Passcode)
}

// FetchConversationsResult 会话拉取结果
type FetchConversationsResult struct {
	Status        string         `json:"status"` // completed / failed / not_logged_in / error
	Conversations []Conversation `json:"conversations"`
	ErrorInfo     string         `json:"error_info,omitempty"`
}

// CheckReplyOptions 判断对方是否回复的选项
type CheckReplyOptions struct {
	TargetURL          string // 对方账号主页链接(主页型平台: TikTok/Instagram/Facebook)
	AccountName        string // 对方账号名(搜索型平台: X/Twitter, 如 @ashly35856)
	Passcode           string // 平台密码验证(如 X 私信 Passcode)
	SinceIncomingCount int    // [已废弃] 保留字段, 当前逻辑改为按"最后一条消息方向"判断, 不再使用该基线
}

// CheckReplyResult 判断回复结果
type CheckReplyResult struct {
	Status      string    `json:"status"`      // completed / failed / not_logged_in / error
	ReplyStatus string    `json:"reply_status"` // replied=对方已回复(最新一条是对方发的) / awaiting_reply=等待对方回复(最新一条是自己发的)
	HasReply    bool      `json:"has_reply"`   // 对方是否已回复(等价于 reply_status == replied)
	ReplyCount  int       `json:"reply_count"` // 本次返回的对方新回复条数
	Replies     []Message `json:"replies"`     // 对方发来的新回复(时间正序)
	CheckedAt   string    `json:"checked_at,omitempty"` // 本次查询的服务端时间(RFC3339)
	ErrorInfo   string    `json:"error_info,omitempty"`
}

const (
	StatusLoggedIn    = "logged_in"
	StatusNotLoggedIn = "not_logged_in"
	StatusAbnormal    = "abnormal"

	defaultMaxConversations = 10
	defaultMaxMessages      = 20
)

// MessengerActions 各平台需实现的私信原子操作接口
type MessengerActions interface {
	// Tag 日志标签, 如 "TT_MSG"
	Tag() string
	// InboxURL 平台私信收件箱页面地址(用于会话列表拉取与登录检测)
	InboxURL() string
	// CheckLogin 检测当前页面登录态, 返回 logged_in / not_logged_in / abnormal
	CheckLogin(ctx context.Context) (string, error)
	// OpenTargetProfile 准备发送入口并导航到位:
	// 主页型平台(TikTok/IG/FB)打开 task.TargetURL 对应主页;
	// 搜索型平台(X)打开私信主页并处理 Passcode 验证等拦截
	OpenTargetProfile(ctx context.Context, task SendTask) error
	// OpenConversationFromProfile 进入与 task 指定目标的会话
	// (主页型平台: 点击主页上的"发消息"按钮; 搜索型平台: 新建会话+搜索目标账号)
	OpenConversationFromProfile(ctx context.Context, task SendTask) error
	// SendInConversation 在已打开的会话中输入内容并点击发送
	SendInConversation(ctx context.Context, content string) error
	// FetchConversationList 从收件箱解析会话列表(按最新在前);
	// 平台可在此处理进入收件箱后的拦截(如 X 的 Passcode 验证)
	FetchConversationList(ctx context.Context, opts FetchOptions) ([]Conversation, error)
	// OpenConversation 从收件箱打开指定会话(依据 FetchConversationList 返回的标识)
	OpenConversation(ctx context.Context, conv Conversation) error
	// FetchConversationMessages 解析当前已打开会话的消息(时间正序: 旧->新)
	FetchConversationMessages(ctx context.Context) ([]Message, error)
}

// RunSend 主动私信统一流程:
// 逐任务: 打开对方主页 -> 登录检测 -> 进入会话 -> 输入并发送 -> 记录结果;
// 任务间随机间隔(8~20秒)降低风控风险; 检测到未登录时终止后续任务。
func RunSend(ctx context.Context, logger *logx.Logger, m MessengerActions, tasks []SendTask) SendResult {
	tag := m.Tag()
	result := SendResult{Status: "completed", Results: make([]SendOutcome, 0, len(tasks))}
	if len(tasks) == 0 {
		result.Status = "error"
		result.ErrorInfo = tag + "0 no send tasks"
		return result
	}
	logger.Print(tag+"1", fmt.Sprintf("开始批量私信流程, 任务数: %d", len(tasks)))

	for i, task := range tasks {
		outcome := SendOutcome{TargetURL: task.TargetURL, AccountName: task.AccountName, Status: "failed"}
		target := task.TargetURL
		if target == "" {
			target = task.AccountName
		}
		logger.Print(tag+"2", fmt.Sprintf("[%d/%d] 打开发送入口: %s", i+1, len(tasks), target))

		if err := m.OpenTargetProfile(ctx, task); err != nil {
			outcome.ErrorInfo = fmt.Sprintf("%s2 打开发送入口失败: %v", tag, err)
			result.Results = append(result.Results, outcome)
			if !waitForNextTask(ctx, logger, tag, i, len(tasks)) {
				break
			}
			continue
		}

		loginStatus, err := m.CheckLogin(ctx)
		if err != nil {
			logger.Print(tag+"2", "登录检测异常: "+err.Error())
		}
		if loginStatus != StatusLoggedIn {
			outcome.Status = StatusNotLoggedIn
			if loginStatus == StatusAbnormal {
				outcome.ErrorInfo = fmt.Sprintf("%s2 账号状态异常", tag)
			} else {
				outcome.ErrorInfo = fmt.Sprintf("%s2 账号未登录", tag)
			}
			result.Results = append(result.Results, outcome)
			result.Status = StatusNotLoggedIn
			logger.Print(tag+"2", "检测到未登录/异常, 终止剩余任务")
			break
		}

		if err := m.OpenConversationFromProfile(ctx, task); err != nil {
			outcome.ErrorInfo = fmt.Sprintf("%s3 进入会话失败: %v", tag, err)
			result.Results = append(result.Results, outcome)
			if !waitForNextTask(ctx, logger, tag, i, len(tasks)) {
				break
			}
			continue
		}

		if err := m.SendInConversation(ctx, task.MessageContent); err != nil {
			outcome.ErrorInfo = fmt.Sprintf("%s4 发送失败: %v", tag, err)
			result.Results = append(result.Results, outcome)
			if !waitForNextTask(ctx, logger, tag, i, len(tasks)) {
				break
			}
			continue
		}

		outcome.Status = "sent"
		result.Results = append(result.Results, outcome)
		logger.Print(tag+"5", fmt.Sprintf("[%d/%d] 私信发送成功", i+1, len(tasks)))
		if !waitForNextTask(ctx, logger, tag, i, len(tasks)) {
			break
		}
	}

	result.Status = summarizeSend(result.Results)
	sentCount := 0
	for _, o := range result.Results {
		if o.Status == "sent" {
			sentCount++
		}
	}
	logger.Print(tag+"6", fmt.Sprintf("批量私信流程结束: 成功 %d/%d, 状态: %s", sentCount, len(tasks), result.Status))
	return result
}

// RunCheckReply 判断对方是否回复的统一流程:
// 打开私信主页 -> 登录检测 -> 搜索账号进入会话 -> 解析消息 -> 过滤对方发来的消息 -> 按增量基线返回新回复。
// 与 RunSend 复用相同的打开会话前置步骤, 差异在于最后一步只读取消息而非发送。
func RunCheckReply(ctx context.Context, logger *logx.Logger, m MessengerActions, opts CheckReplyOptions) CheckReplyResult {
	tag := m.Tag()
	checkedAt := time.Now().Format(time.RFC3339)
	result := CheckReplyResult{Status: "completed", Replies: []Message{}, CheckedAt: checkedAt}

	task := SendTask{
		TargetURL:   opts.TargetURL,
		AccountName: opts.AccountName,
		Passcode:    opts.Passcode,
	}
	target := opts.TargetURL
	if target == "" {
		target = opts.AccountName
	}
	logger.Print(tag+"R1", "开始判断回复流程: "+target)

	if err := m.OpenTargetProfile(ctx, task); err != nil {
		result.Status = "failed"
		result.ErrorInfo = fmt.Sprintf("%sR2 打开私信主页失败: %v", tag, err)
		return result
	}

	loginStatus, err := m.CheckLogin(ctx)
	if err != nil {
		logger.Print(tag+"R2", "登录检测异常: "+err.Error())
	}
	if loginStatus != StatusLoggedIn {
		result.Status = StatusNotLoggedIn
		if loginStatus == StatusAbnormal {
			result.ErrorInfo = fmt.Sprintf("%sR2 账号状态异常", tag)
		} else {
			result.ErrorInfo = fmt.Sprintf("%sR2 账号未登录", tag)
		}
		return result
	}

	if err := m.OpenConversationFromProfile(ctx, task); err != nil {
		result.Status = "failed"
		result.ErrorInfo = fmt.Sprintf("%sR3 进入会话失败: %v", tag, err)
		return result
	}

	msgs, err := m.FetchConversationMessages(ctx)
	if err != nil {
		result.Status = "failed"
		result.ErrorInfo = fmt.Sprintf("%sR4 解析会话消息失败: %v", tag, err)
		return result
	}

	// 核心判断: 最后一条消息是谁发的。
	// 若最后一条是自己发出(outgoing) => 对方还没回复我的最新消息;
	// 若最后一条是对方发出(incoming) => 对方已回复。
	lastOutgoing := false
	lastOutgoingIdx := -1
	for i, msg := range msgs {
		if msg.Direction == "outgoing" {
			lastOutgoing = true
			lastOutgoingIdx = i
		} else if msg.Direction == "incoming" {
			lastOutgoing = false
		}
	}

	if lastOutgoing || len(msgs) == 0 {
		// 等待对方回复(或会话尚无消息)
		result.ReplyStatus = "awaiting_reply"
		result.HasReply = false
		result.ReplyCount = 0
		result.Replies = []Message{}
		logger.Print(tag+"R5", "判断回复完成: 对方尚未回复(最后一条为自己发出)")
		return result
	}

	// 对方已回复: 返回"最后一次自己发言之后"对方发来的所有消息(时间正序)。
	// 这样天然只对应最新一轮对话, 不依赖调用方维护增量基线, 也不会返回前期旧回复。
	replies := make([]Message, 0)
	for i := lastOutgoingIdx + 1; i < len(msgs); i++ {
		if msgs[i].Direction == "incoming" {
			msgs[i].ObservedAt = checkedAt
			replies = append(replies, msgs[i])
		}
	}
	if replies == nil {
		replies = []Message{}
	}

	result.ReplyStatus = "replied"
	result.HasReply = len(replies) > 0
	result.ReplyCount = len(replies)
	result.Replies = replies
	logger.Print(tag+"R5", fmt.Sprintf("判断回复完成: 对方已回复, 返回 %d 条新回复", result.ReplyCount))
	return result
}

// waitForNextTask 任务间随机等待8~20秒降低风控风险(最后一个任务不等待), 返回false表示ctx已取消应终止循环
func waitForNextTask(ctx context.Context, logger *logx.Logger, tag string, i, total int) bool {
	if i >= total-1 {
		return true
	}
	wait := time.Duration(8+rand.Intn(13)) * time.Second
	logger.Print(tag+"5", fmt.Sprintf("随机等待 %v 后处理下一个任务", wait))
	select {
	case <-time.After(wait):
		return true
	case <-ctx.Done():
		logger.Print(tag+"5", "上下文超时/取消, 终止剩余任务: "+ctx.Err().Error())
		return false
	}
}

// summarizeSend 汇总整体状态
func summarizeSend(outcomes []SendOutcome) string {
	if len(outcomes) == 0 {
		return "error"
	}
	sent, failed := 0, 0
	for _, o := range outcomes {
		switch o.Status {
		case "sent":
			sent++
		case StatusNotLoggedIn:
			return StatusNotLoggedIn
		default:
			failed++
		}
	}
	switch {
	case sent == len(outcomes):
		return "completed"
	case sent == 0:
		return "failed"
	default:
		return "partial_failed"
	}
}

// RunFetchConversations 会话列表拉取统一流程:
// 打开收件箱 -> 登录检测 -> 解析会话列表 -> 逐个打开会话解析最新消息 -> 汇总清洗返回。
func RunFetchConversations(ctx context.Context, logger *logx.Logger, m MessengerActions, opts FetchOptions) FetchConversationsResult {
	tag := m.Tag()
	result := FetchConversationsResult{Status: "completed", Conversations: []Conversation{}}
	if opts.MaxConversations <= 0 {
		opts.MaxConversations = defaultMaxConversations
	}
	if opts.MaxMessagesPerConversation <= 0 {
		opts.MaxMessagesPerConversation = defaultMaxMessages
	}

	logger.Print(tag+"1", fmt.Sprintf("开始会话拉取流程, 最多 %d 个会话, 每会话最新 %d 条", opts.MaxConversations, opts.MaxMessagesPerConversation))
	logger.Print(tag+"2", "打开收件箱: "+m.InboxURL())
	if err := chromedp.Run(ctx, chromedp.Navigate(m.InboxURL()), chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
		result.Status = "error"
		result.ErrorInfo = fmt.Sprintf("%s2 打开收件箱失败: %v", tag, err)
		return result
	}
	time.Sleep(3 * time.Second)

	loginStatus, err := m.CheckLogin(ctx)
	if err != nil {
		logger.Print(tag+"2", "登录检测异常: "+err.Error())
	}
	if loginStatus != StatusLoggedIn {
		result.Status = StatusNotLoggedIn
		if loginStatus == StatusAbnormal {
			result.ErrorInfo = fmt.Sprintf("%s2 账号状态异常", tag)
		} else {
			result.ErrorInfo = fmt.Sprintf("%s2 账号未登录", tag)
		}
		return result
	}

	logger.Print(tag+"3", "解析会话列表")
	convs, err := m.FetchConversationList(ctx, opts)
	if err != nil {
		result.Status = "error"
		result.ErrorInfo = fmt.Sprintf("%s3 解析会话列表失败: %v", tag, err)
		return result
	}
	if len(convs) > opts.MaxConversations {
		convs = convs[:opts.MaxConversations]
	}
	logger.Print(tag+"3", fmt.Sprintf("解析到 %d 个会话(截取前 %d 个)", len(convs), opts.MaxConversations))

	for i := range convs {
		logger.Print(tag+"4", fmt.Sprintf("[%d/%d] 打开会话: %s", i+1, len(convs), convs[i].PartnerName))
		if err := m.OpenConversation(ctx, convs[i]); err != nil {
			logger.Print(tag+"4", fmt.Sprintf("打开会话失败, 跳过: %v", err))
			continue
		}
		msgs, err := m.FetchConversationMessages(ctx)
		if err != nil {
			logger.Print(tag+"5", fmt.Sprintf("解析消息失败, 跳过: %v", err))
			continue
		}
		if len(msgs) > opts.MaxMessagesPerConversation {
			msgs = msgs[len(msgs)-opts.MaxMessagesPerConversation:]
		}
		convs[i].Messages = msgs
		if i < len(convs)-1 {
			wait := time.Duration(2+rand.Intn(4)) * time.Second
			logger.Print(tag+"5", fmt.Sprintf("会话处理完成, 等待 %v 后继续", wait))
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				result.Status = "error"
				result.ErrorInfo = fmt.Sprintf("%s5 上下文超时/取消: %v", tag, ctx.Err())
				result.Conversations = sanitizeConversations(convs)
				return result
			}
		}
	}

	result.Conversations = sanitizeConversations(convs)
	logger.Print(tag+"6", fmt.Sprintf("会话拉取流程结束, 返回 %d 个会话, 状态: %s", len(result.Conversations), result.Status))
	return result
}

// sanitizeConversations 清洗会话数据中的非法UTF-8字符, 防止Rails端编码错误
func sanitizeConversations(convs []Conversation) []Conversation {
	for i := range convs {
		convs[i].ConversationID = scraper.SanitizeString(convs[i].ConversationID)
		convs[i].PartnerName = scraper.SanitizeString(convs[i].PartnerName)
		convs[i].PartnerURL = scraper.SanitizeString(convs[i].PartnerURL)
		convs[i].LastMessage = scraper.SanitizeString(convs[i].LastMessage)
		convs[i].LastMessageAt = scraper.SanitizeString(convs[i].LastMessageAt)
		for j := range convs[i].Messages {
			convs[i].Messages[j].SenderName = scraper.SanitizeString(convs[i].Messages[j].SenderName)
			convs[i].Messages[j].Content = scraper.SanitizeString(convs[i].Messages[j].Content)
			convs[i].Messages[j].SentAt = scraper.SanitizeString(convs[i].Messages[j].SentAt)
		}
	}
	return convs
}
