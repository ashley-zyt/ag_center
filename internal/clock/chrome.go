package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"minimax_pro/internal/logx"
	"minimax_pro/internal/undetectable"
)

// LockedProfileInfo 用于承载被锁定的浏览器简要信息
type LockedProfileInfo struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
}

// APIResponse 统一的接口返回格式
type APIResponse struct {
	Code    int                 `json:"code"`
	Message string              `json:"message"`
	Data    []LockedProfileInfo `json:"data"`
}

// GetLockedProfiles 获取当前所有状态为 Locked (经释放后仍死锁) 的指纹浏览器列表
func GetLockedProfiles(ctx context.Context, logger *logx.Logger, host string, port int) ([]LockedProfileInfo, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 25325
	}

	client := undetectable.NewClient(host, port)

	localCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	logger.Print("API", "[第一轮] 正在调取 Undetectable API 获取全量浏览器列表...")
	profiles, err := client.ListProfiles(localCtx)
	if err != nil {
		return nil, fmt.Errorf("第一轮获取 Profile 列表失败: %v", err)
	}

	// 1. 💡 修正判定范围：无论是正在运行的(Started) 还是已经死锁的(Locked)，全部纳入释放嫌疑
	var suspectIDs []string
	for id, info := range profiles {
		if info.Status == "Started" || info.Status == "Locked" {
			suspectIDs = append(suspectIDs, id)
			logger.Print("API", fmt.Sprintf("发现潜在占用浏览器: %s, 当前状态: %s", info.Name, info.Status))
		}
	}

	// 如果没有任何被占用的浏览器，直接返回空列表
	if len(suspectIDs) == 0 {
		logger.Print("API", "第一轮检查未发现任何 Started 或 Locked 状态的浏览器")
		return []LockedProfileInfo{}, nil
	}

	logger.Print("API", fmt.Sprintf("开始对 %d 个嫌疑浏览器批量发送停止释放指令...", len(suspectIDs)))

	// 2. 对所有嫌疑浏览器依次发送 Stop 指令，强行关闭正在运行的，或尝试解开已死锁的
	for _, id := range suspectIDs {
		logger.Print("API", fmt.Sprintf("发送停止指令 -> ProfileID: %s", id))
		_ = client.StopProfileBestEffort(localCtx, id)
	}

	// 💡 给指纹浏览器关闭内核、杀掉进程、以及同步云端留出 4 秒的充裕时间
	logger.Print("API", "已发送全部停止指令，安全等待 4 秒进行进程与状态同步...")
	time.Sleep(4 * time.Second)

	// 3. 重新获取一次全量列表进行最终的“真死锁”判定
	logger.Print("API", "[第二轮] 重新拉取最新的全量浏览器列表...")
	verifiedProfiles, err := client.ListProfiles(localCtx)
	if err != nil {
		return nil, fmt.Errorf("第二轮获取 Profile 列表失败: %v", err)
	}

	var finalLockedList []LockedProfileInfo
	for _, id := range suspectIDs {
		if info, ok := verifiedProfiles[id]; ok {
			// 💡 最终过滤：经过 Stop 指令以及 4 秒等待后：
			// 如果原本是 Started 变成了 Available，说明成功被程序关闭释放了（不返回给前端）
			// 如果状态依然顽固保持在 "Locked" 或未能成功关闭，说明是真正的云端锁死，必须人工干预！
			if info.Status == "Locked" || info.Status == "Started" {
				finalLockedList = append(finalLockedList, LockedProfileInfo{
					ProfileID: id,
					Name:      info.Name,
				})
				logger.Print("API", fmt.Sprintf("🚨 确认真死锁/无法关闭的浏览器: %s (%s), 当前状态: %s", info.Name, id, info.Status))
			} else {
				logger.Print("API", fmt.Sprintf("正常：已成功关闭并释放浏览器: %s (%s)", info.Name, id))
			}
		}
	}

	logger.Print("API", fmt.Sprintf("二次核对完毕，当前共有 %d 个确认无法自动解除锁定的浏览器", len(finalLockedList)))
	return finalLockedList, nil
}

// GetLockedProfilesHandler 外部 HTTP 调用的 Handler
func GetLockedProfilesHandler(logger *logx.Logger, host string, port int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(APIResponse{Code: 405, Message: "仅支持 GET 请求", Data: nil})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()

		lockedProfiles, err := GetLockedProfiles(ctx, logger, host, port)
		if err != nil {
			logger.Print("API_ERR", "接口查询失败: "+err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(APIResponse{Code: 500, Message: "查询指纹浏览器状态失败: " + err.Error(), Data: nil})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(APIResponse{
			Code:    200,
			Message: "success",
			Data:    lockedProfiles,
		})
	}
}
