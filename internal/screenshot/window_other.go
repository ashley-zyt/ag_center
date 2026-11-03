//go:build !windows

package screenshot

import "fmt"

// CaptureWindow 在非 Windows 平台上不可用（本项目部署于 Windows）。
func CaptureWindow(websocketURL string) ([]byte, error) {
	return nil, fmt.Errorf("系统级窗口截屏仅支持 Windows")
}
