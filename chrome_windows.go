//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// ---------- DWM ----------

const (
	dwmwaUseImmersiveDarkMode   = 20
	dwmwaWindowCornerPreference = 33
	dwmwaBorderColor            = 34
	dwmwaCaptionColor           = 35
	dwmwaTextColor              = 36

	dwmwcpRound = 2 // DWMWCP_ROUND：去掉非客户区后需显式要回 Win11 圆角
)

// ---------- Win32 ----------

const (
	wmNCCalcSize    = 0x0083
	wmNCHitTest     = 0x0084
	wmNCLButtonDown = 0x00A1
	wmGetMinMaxInfo = 0x0024
	wmClose         = 0x0010

	htClient      = 1
	htCaption     = 2
	htLeft        = 10
	htRight       = 11
	htTop         = 12
	htTopLeft     = 13
	htTopRight    = 14
	htBottom      = 15
	htBottomLeft  = 16
	htBottomRight = 17

	swpFrameChanged = 0x0020
	swpNoMove       = 0x0002
	swpNoSize       = 0x0001
	swpNoZOrder     = 0x0004

	swMinimize = 6
	swMaximize = 3
	swRestore  = 9

	smCXFrame        = 32
	smCXPaddedBorder = 92

	// 最小窗口尺寸（96 DPI 下的逻辑像素）。
	// 宽度下限来自布局：侧边栏 212 + 内容区左右内边距 52 + 卡片可用宽度约 420。
	// 再窄输入框会挤得没法用；高度下限保证侧边栏所有条目加状态不被截断。
	minWindowW = 720
	minWindowH = 520
)

var (
	dwmapi = syscall.NewLazyDLL("dwmapi.dll")
	user32 = syscall.NewLazyDLL("user32.dll")

	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	procSetWindowLongPtr      = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc        = user32.NewProc("CallWindowProcW")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	procIsZoomed              = user32.NewProc("IsZoomed")
	procShowWindow            = user32.NewProc("ShowWindow")
	procPostMessage           = user32.NewProc("PostMessageW")
	procSendMessage           = user32.NewProc("SendMessageW")
	procReleaseCapture        = user32.NewProc("ReleaseCapture")
	procLoadImage             = user32.NewProc("LoadImageW")
	procGetModuleHandle       = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
	procGetDpiForWindow       = user32.NewProc("GetDpiForWindow")
)

type rect struct{ Left, Top, Right, Bottom int32 }

type point struct{ X, Y int32 }

// MINMAXINFO：WM_GETMINMAXINFO 的 lParam 指向它。
type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

// windowDPI 返回窗口所在显示器的 DPI。
// GetDpiForWindow 需要 Win10 1607+，取不到时按 96 处理。
func windowDPI(hwnd uintptr) int32 {
	if procGetDpiForWindow.Find() != nil {
		return 96
	}
	r, _, _ := procGetDpiForWindow.Call(hwnd)
	if r == 0 {
		return 96
	}
	return int32(r)
}

// GWLP_WNDPROC 为负值，必须用变量做运行时转换：
// uintptr(常量 -4) 在编译期会被判定为溢出。
var gwlpWndProc = int32(-4)

var origWndProc uintptr

func dwmSetUint32(hwnd uintptr, attr uint32, v uint32) {
	// 系统版本过低时静默失败，退回默认外观。
	_, _, _ = procDwmSetWindowAttribute.Call(
		hwnd, uintptr(attr), uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
}

// colorref 组装 Win32 COLORREF：0x00BBGGRR。
func colorref(r, g, b uint32) uint32 { return r | g<<8 | b<<16 }

func systemUsesDarkTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`,
		registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return false
	}
	return v == 0
}

func getSystemMetrics(i int32) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int32(r)
}

func isMaximized(hwnd uintptr) bool {
	r, _, _ := procIsZoomed.Call(hwnd)
	return r != 0
}

// wndProc 只接管非客户区计算，其余转回原窗口过程。
//
// 注意：这里不做 WM_NCHITTEST。WebView2 是一个铺满客户区的子窗口，
// 鼠标消息全被它接收，父窗口的命中测试根本不会被调用。
// 拖拽与缩放改由界面侧 JS 检测后回调 windowCommand 实现。
func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmNCCalcSize:
		if wparam != 0 {
			// 返回 0 且不改动矩形 => 客户区铺满整个窗口，标题栏消失。
			if isMaximized(hwnd) {
				// 最大化时若不内缩，内容会溢出屏幕并盖住任务栏。
				inset := getSystemMetrics(smCXFrame) + getSystemMetrics(smCXPaddedBorder)
				r := (*rect)(unsafe.Pointer(lparam))
				r.Left += inset
				r.Right -= inset
				r.Bottom -= inset
				r.Top += inset
			}
			return 0
		}

	case wmGetMinMaxInfo:
		// 限制最小尺寸，避免布局被拖垮。按窗口所在显示器的 DPI 缩放，
		// 否则在 150% / 200% 缩放的屏幕上这个下限会形同虚设。
		dpi := windowDPI(hwnd)
		mmi := (*minMaxInfo)(unsafe.Pointer(lparam))
		mmi.MinTrackSize.X = minWindowW * dpi / 96
		mmi.MinTrackSize.Y = minWindowH * dpi / 96
		return 0
	}

	r, _, _ := procCallWindowProc.Call(origWndProc, hwnd, msg, wparam, lparam)
	return r
}

// applyWindowChrome 去掉系统标题栏，恢复 Win11 圆角，
// 并让边框配色跟随应用而非系统强调色。
func applyWindowChrome(hwnd uintptr) {
	dark := systemUsesDarkTheme()

	var immersive uint32
	if dark {
		immersive = 1
	}
	dwmSetUint32(hwnd, dwmwaUseImmersiveDarkMode, immersive)

	// 去掉非客户区会连带丢掉 Win11 的圆角，这里显式要回来。
	dwmSetUint32(hwnd, dwmwaWindowCornerPreference, dwmwcpRound)

	if dark {
		dwmSetUint32(hwnd, dwmwaCaptionColor, colorref(0x20, 0x20, 0x22))
		dwmSetUint32(hwnd, dwmwaTextColor, colorref(0xf5, 0xf5, 0xf7))
		dwmSetUint32(hwnd, dwmwaBorderColor, colorref(0x3a, 0x3a, 0x3c))
	} else {
		dwmSetUint32(hwnd, dwmwaCaptionColor, colorref(0xe9, 0xe9, 0xeb))
		dwmSetUint32(hwnd, dwmwaTextColor, colorref(0x1d, 0x1d, 0x1f))
		dwmSetUint32(hwnd, dwmwaBorderColor, colorref(0xd6, 0xd6, 0xda))
	}

	origWndProc, _, _ = procSetWindowLongPtr.Call(
		hwnd, uintptr(gwlpWndProc), syscall.NewCallback(wndProc))

	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		uintptr(swpFrameChanged|swpNoMove|swpNoSize|swpNoZOrder))
}

// applyWindowIcon 把嵌入 exe 的图标资源挂到窗口上。
//
// go-webview2 自建窗口类且不指定 hIcon，任务栏与 Alt+Tab 取的是窗口图标，
// 不设的话即使 exe 有资源图标，运行中的窗口仍是默认图标。
func applyWindowIcon(hwnd uintptr) {
	const (
		imageIcon     = 1
		lrDefaultSize = 0x0040
		lrShared      = 0x8000
		wmSetIcon     = 0x0080
		iconSmall     = 0
		iconBig       = 1
	)
	hInst, _, _ := procGetModuleHandle.Call(0)
	// rsrc 生成的资源里，主图标 ID 固定为 1
	for _, which := range []uintptr{iconSmall, iconBig} {
		cx, cy := uintptr(16), uintptr(16)
		if which == iconBig {
			cx, cy = 32, 32
		}
		h, _, _ := procLoadImage.Call(hInst, 1, imageIcon, cx, cy, lrShared)
		if h == 0 {
			h, _, _ = procLoadImage.Call(hInst, 1, imageIcon, 0, 0, lrDefaultSize|lrShared)
		}
		if h != 0 {
			procSendMessage.Call(hwnd, wmSetIcon, which, h)
		}
	}
}

// resizeHT 把界面传来的方向映射到 Win32 命中测试常量。
var resizeHT = map[string]uintptr{
	"l": htLeft, "r": htRight, "t": htTop, "b": htBottom,
	"tl": htTopLeft, "tr": htTopRight, "bl": htBottomLeft, "br": htBottomRight,
}

// windowCommand 供界面调用。
//
// drag / resize 走 ReleaseCapture + WM_NCLBUTTONDOWN，把控制权交还系统，
// 进入原生的移动/缩放模态循环。这样 Snap 分屏、边缘吸附、拖拽预览、
// 缩放时的实时重绘全部是系统行为，不需要自己模拟。
func windowCommand(hwnd uintptr, action string) {
	switch {
	case action == "minimize":
		procShowWindow.Call(hwnd, uintptr(swMinimize))

	case action == "maximize":
		if isMaximized(hwnd) {
			procShowWindow.Call(hwnd, uintptr(swRestore))
		} else {
			procShowWindow.Call(hwnd, uintptr(swMaximize))
		}

	case action == "close":
		procPostMessage.Call(hwnd, uintptr(wmClose), 0, 0)

	case action == "drag":
		procReleaseCapture.Call()
		procSendMessage.Call(hwnd, uintptr(wmNCLButtonDown), uintptr(htCaption), 0)

	case strings.HasPrefix(action, "resize:"):
		if ht, ok := resizeHT[strings.TrimPrefix(action, "resize:")]; ok {
			procReleaseCapture.Call()
			procSendMessage.Call(hwnd, uintptr(wmNCLButtonDown), ht, 0)
		}
	}
}
