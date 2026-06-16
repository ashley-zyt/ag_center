package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/chromedp/chromedp"
)

// ReportPayload 定义上报 Rails 的数据结构
type ReportPayload struct {
	ID          int    `json:"id"`
	TaskType    string `json:"task_type"`
	Status      string `json:"status"`
	PublishTime string `json:"publish_time"` // 新增采集的时间字段
	StatusDesp  string `json:"status_desp,omitempty"`
}

func main() {
	// 1. 初始化 chromedp 上下文
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()

	// 设置超时
	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 假设这里是当前正在执行的任务 ID
	currentTaskID := 19081

	// 2. 执行配置好的浏览器动作（此处以导航到视频列表页为例）
	// 注意：实际运行中，你可能是在发布完视频后停留在当前页面，或直接刷新列表
	var rawDateText string
	err := chromedp.Run(ctx,
		// 确保视频行容器已加载
		chromedp.WaitVisible(`ytcp-video-row`, chromedp.ByQuery),

		// 使用 JS 精准提取第一行视频的真实日期文本
		// ytcp-video-row 里的 .tablecell-date 包含了日期和状态。我们通过 split('\n')[0] 只要第一行。
		chromedp.Evaluate(`
			(func() {
				const dateCell = document.querySelector('ytcp-video-row .tablecell-date');
				if (!dateCell) return '';
				return dateCell.innerText.split('\n')[0].trim();
			})()
		`, &rawDateText),
	)

	if err != nil {
		log.Printf("DOM 采集失败: %v", err)
		reportToRails(currentTaskID, "failed", "", fmt.Sprintf("DOM 采集失败: %v", err))
		return
	}

	log.Printf("成功采集到发布时间文本: [%s]", rawDateText)

	// 3. 将采集到的状态上报给 Rails
	if rawDateText != "" {
		reportToRails(currentTaskID, "success", rawDateText, "")
	} else {
		reportToRails(currentTaskID, "failed", "", "未能从 DOM 中获取到有效的发布时间")
	}
}

// 向上报接口发送 POST 请求
func reportToRails(taskID int, status string, publishTime string, statusDesp string) {
	apiURL := "http://localhost:3000/api/v1/tasks/report" // 请根据实际情况修改域名

	payload := ReportPayload{
		ID:          taskID,
		TaskType:    "operation", // 或者是 "move", 根据你当下的任务类型决定
		Status:      status,
		PublishTime: publishTime,
		StatusDesp:  statusDesp,
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("JSON 序列化失败: %v", err)
		return
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		log.Printf("创建请求失败: %v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("上报 Rails 失败: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Println("Rails 状态上报成功！")
	} else {
		log.Printf("Rails 响应异常，状态码: %d", resp.StatusCode)
	}
}
