//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	shell32          = syscall.NewLazyDLL("shell32.dll")
	procShellExecute = shell32.NewProc("ShellExecuteW")
)

// openInFileManager 在资源管理器中打开目录。
//
// 不能用 hideConsole 包装 explorer.exe：那个辅助函数会设 HideWindow，
// 翻译成 STARTF_USESHOWWINDOW + SW_HIDE 传给新进程。对 taskkill 这类
// 控制台程序，它隐藏的是黑窗；对 explorer.exe 这种 GUI 程序，
// 隐藏的就是它唯一要展示的那个文件夹窗口——点了没反应正是这么来的。
//
// 改用 ShellExecuteW + open 动词：这是打开路径的规范做法，走用户默认的
// 文件管理器，并且能正确处理 \\wsl.localhost\... 这类 UNC 路径。
func openInFileManager(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	verb, _ := syscall.UTF16PtrFromString("open")

	const swShowNormal = 1
	r, _, lastErr := procShellExecute.Call(
		0,                             // hwnd
		uintptr(unsafe.Pointer(verb)), // lpOperation
		uintptr(unsafe.Pointer(p)),    // lpFile
		0,                             // lpParameters
		0,                             // lpDirectory
		swShowNormal,                  // nShowCmd
	)
	// 返回值是历史遗留的 HINSTANCE 语义：大于 32 才算成功，
	// 小于等于 32 的都是错误码。
	if r <= 32 {
		return fmt.Errorf("打开 %s 失败 (code %d): %v", path, r, lastErr)
	}
	return nil
}
