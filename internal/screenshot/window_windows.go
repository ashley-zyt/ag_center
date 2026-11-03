//go:build windows

package screenshot

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// 系统级窗口截屏：CDP 的 page.CaptureScreenshot 在 Undetectable 指纹浏览器上会挂起，
// 因此退而用 Windows GDI 的 PrintWindow 截取对应浏览器窗口。
//
// 定位思路：每个 profile 的浏览器进程命令行里带 --remote-debugging-port=<port>，
// 用它唯一定位进程 → 枚举该进程的可见主窗口 → PrintWindow 截取窗口内容。

var (
	user32 = syscall.NewLazyDLL("user32.dll")
	gdi32  = syscall.NewLazyDLL("gdi32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procPrintWindow              = user32.NewProc("PrintWindow")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
)

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

const (
	pwRenderFullContent = 2          // PrintWindow 标志：渲染完整内容（含 GPU 加速层）
	dibRGBColors        = 0          // CreateDIBSection 的 uUsage
	biRGB               = 0          // BITMAPINFOHEADER 的 biCompression
	srcCopy             = 0x00CC0020 // BitBlt 的光栅操作码 SRCCOPY
)

// CaptureWindow 截取指定调试端口对应的浏览器窗口，返回 PNG 字节。
// websocketURL 形如 ws://127.0.0.1:45678/devtools/browser/<id>，据此解析出调试端口。
func CaptureWindow(websocketURL string) ([]byte, error) {
	port := debugPortFromWebsocket(websocketURL)
	if port == "" {
		return nil, fmt.Errorf("无法从 websocket 地址解析调试端口: %q", websocketURL)
	}
	pid, err := findBrowserPID(port)
	if err != nil {
		return nil, err
	}
	hwnd, err := findMainWindow(pid)
	if err != nil {
		return nil, err
	}
	return captureWindow(hwnd)
}

// debugPortFromWebsocket 从 CDP websocket 地址解析调试端口。
func debugPortFromWebsocket(websocketURL string) string {
	s := strings.TrimSpace(websocketURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// findBrowserPID 通过调试端口定位浏览器进程 PID（进程命令行含 --remote-debugging-port=<port>）。
func findBrowserPID(debugPort string) (uint32, error) {
	// 不按进程名过滤：Undetectable 的浏览器进程名不固定（可能是 Undetected.exe / chrome.exe 等），
	// 只按调试端口特征匹配命令行。端口号唯一，足以精确定位到对应 profile 的浏览器进程。
	script := fmt.Sprintf(
		`$ps = Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*--remote-debugging-port=%s*' }; if ($ps) { $ps[0].ProcessId } else { 'NO_MATCH' }`,
		debugPort)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return 0, fmt.Errorf("定位浏览器进程失败: %v", err)
	}
	pidStr := strings.TrimSpace(string(out))
	if pidStr == "" || pidStr == "NO_MATCH" {
		return 0, fmt.Errorf("未找到调试端口 %s 对应的浏览器进程", debugPort)
	}
	pid, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("解析 PID 失败: %s", pidStr)
	}
	return uint32(pid), nil
}

// findMainWindow 枚举窗口，找到指定 PID 的可见主窗口句柄（取面积最大的可见窗口）。
func findMainWindow(pid uint32) (syscall.Handle, error) {
	var found syscall.Handle
	var maxArea int64 = -1
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		var wpid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&wpid)))
		if wpid != pid {
			return 1 // 继续枚举
		}
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		if vis == 0 {
			return 1
		}
		var r winRect
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
		area := int64(r.Right-r.Left) * int64(r.Bottom-r.Top)
		if area > maxArea {
			maxArea = area
			found = syscall.Handle(hwnd)
		}
		return 1 // 继续枚举，找面积最大的主窗口
	})
	procEnumWindows.Call(cb, 0)
	if found == 0 {
		return 0, fmt.Errorf("未找到 PID %d 的可见窗口", pid)
	}
	return found, nil
}

// captureWindow 用 PrintWindow 截取指定窗口内容，返回 PNG 字节。
func captureWindow(hwnd syscall.Handle) ([]byte, error) {
	var r winRect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	w := int(r.Right - r.Left)
	h := int(r.Bottom - r.Top)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("窗口尺寸无效: %dx%d", w, h)
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return nil, fmt.Errorf("获取屏幕 DC 失败")
	}
	defer procReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return nil, fmt.Errorf("创建内存 DC 失败")
	}
	defer procDeleteDC.Call(hdcMem)

	// 用 CreateDIBSection 直接创建 32 位 DIB 并拿到像素内存指针，避免 GetDIBits 转换失败。
	bmi := bitmapInfoHeader{
		BiSize:        40,
		BiWidth:       int32(w),
		BiHeight:      -int32(h), // 负值 = 自顶向下
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: biRGB,
	}
	var bits unsafe.Pointer
	hbmp, _, _ := procCreateDIBSection.Call(hdcMem, uintptr(unsafe.Pointer(&bmi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 || bits == nil {
		return nil, fmt.Errorf("创建 DIB 位图失败")
	}
	defer procDeleteObject.Call(hbmp)

	oldObj, _, _ := procSelectObject.Call(hdcMem, hbmp)
	defer procSelectObject.Call(hdcMem, oldObj)

	// 优先 BitBlt 从屏幕拷贝窗口区域：它截取屏幕合成后的最终画面（含 GPU 加速的页面内容），
	// 比 PrintWindow 更准确（PrintWindow 对 Chromium 的 DirectComposition 常只截到地址栏/部分页面）。
	// BitBlt 失败（如窗口不可见/被遮挡）时再用 PrintWindow 兜底。
	bitBltOK, _, _ := procBitBlt.Call(hdcMem, 0, 0, uintptr(w), uintptr(h), hdcScreen, uintptr(r.Left), uintptr(r.Top), srcCopy)
	if bitBltOK == 0 {
		ok, _, _ := procPrintWindow.Call(uintptr(hwnd), hdcMem, pwRenderFullContent)
		if ok == 0 {
			return nil, fmt.Errorf("窗口截取失败")
		}
	}

	// bits 直接指向 32 位 BGRA 像素（自顶向下）。
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pix := unsafe.Slice((*byte)(bits), w*h*4)
	copy(img.Pix, pix)
	// BGRA -> RGBA
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+2] = img.Pix[i+2], img.Pix[i]
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
