//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 控制面板的单实例保护。
//
// 加这个不只是为了 WebView2 目录能安全删除。两个面板同时开着，各自读一份
// 配置、各自保存，就是后写的覆盖先写的——用户在另一个窗口里改的上游、
// 槽位会无声消失。控制面板本来就该只有一个。
//
// 后台代理不受影响：它是同一个 exe 的另一种模式，不走这条路径。
var (
	procFindWindow          = user32.NewProc("FindWindowW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

const (
	// go-webview2 建窗口时用的标题，见 runGUI 里的 WindowOptions.Title。
	guiWindowTitle = "ccproxy"
	guiMutexName   = `Local\ccproxy-gui`
)

// acquireGUISingleInstance 尝试取得「唯一控制面板」这个身份。
//
// ok 为 false 表示已经有一个面板在运行——此时已把那个窗口提到前台，
// 本进程该安静退出。直接报错弹窗是更差的选择：用户的意图是「打开面板」，
// 而面板已经在那儿了，把它显示出来才是他要的结果。
func acquireGUISingleInstance() (release func(), ok bool) {
	name, err := syscall.UTF16PtrFromString(guiMutexName)
	if err != nil {
		return func() {}, true // 取不到锁时宁可放行，也不要挡住用户开面板
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		// 句柄仍然有效，必须关掉，否则本进程会给互斥量多留一份引用。
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		focusExistingGUI()
		return func() {}, false
	}
	if err != nil || h == 0 {
		return func() {}, true
	}
	return func() { _ = windows.CloseHandle(h) }, true
}

// focusExistingGUI 把已有的面板窗口提到前台。
func focusExistingGUI() {
	title, err := syscall.UTF16PtrFromString(guiWindowTitle)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	// 窗口可能是最小化的，先还原再前置——只调 SetForegroundWindow
	// 对最小化窗口无效，表现为「点了图标什么都没发生」。
	procShowWindow.Call(hwnd, swRestore)
	procSetForegroundWindow.Call(hwnd)
}

// removeWebView2Profile 删除 WebView2 的用户数据目录。
//
// WebView2 必须有这么一个目录，关不掉；但里面全是可再生的 Chromium 缓存
// （实测 114 个文件 / 12 MB），ccproxy 自己没有任何状态存在其中——
// 配置在 config.json 里。目录建在系统临时目录下，面板一关就删。
//
// 退出时这一次是尽力而为：w.Destroy() 返回之后，WebView2 派生的
// msedgewebview2.exe 还要一段时间才退干净，此前文件仍被占用。原先给的
// 预算是 2 秒，实测不够——卸载后目录仍留在 %TEMP% 下，因为 10 次重试
// 全落在子进程还活着的窗口里，等它们退干净时已经没人再来删了。
//
// 现在给 5 秒。再长就没有意义：窗口早已消失，进程却还在，用户会以为
// 程序没退干净。真的删不掉也不报错——两条兜底都成立：下次启动会先删一次
// （那时必定无人占用），而这里是系统临时目录，本来就归系统的清理机制管。
func removeWebView2Profile(dir string) {
	if dir == "" {
		return
	}
	for i := 0; i < 25; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
