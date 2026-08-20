//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	advapi32              = syscall.NewLazyDLL("advapi32.dll")
	procOpenProcessToken  = advapi32.NewProc("OpenProcessToken")
	procGetTokenInfo      = advapi32.NewProc("GetTokenInformation")
	kernel32b             = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentProcess = kernel32b.NewProc("GetCurrentProcess")
)

const (
	tokenQuery     = 0x0008
	tokenElevation = 20 // TOKEN_INFORMATION_CLASS.TokenElevation
)

// IsElevated 判断当前进程是否以管理员权限运行。
//
// 为什么要检测：Windows 的 UIPI 规定，非提权进程的全局键盘钩子
// 在**提权窗口获得焦点时收不到按键**。所以一旦本程序被提权启动，
// 用户的全局热键（截图、录屏等）只要焦点落在本窗口就会失灵——
// 而且现象完全不指向本程序，极难排查。
//
// 提权通常不是有意的：从管理员终端启动会直接继承权限，
// 尽管本程序从不请求管理员权限，清单里也没有 requireAdministrator。
func IsElevated() bool {
	hProc, _, _ := procGetCurrentProcess.Call()
	var token syscall.Handle
	ret, _, _ := procOpenProcessToken.Call(hProc, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if ret == 0 {
		return false
	}
	defer syscall.CloseHandle(token)

	var elevated uint32
	var retLen uint32
	ret, _, _ = procGetTokenInfo.Call(
		uintptr(token), tokenElevation,
		uintptr(unsafe.Pointer(&elevated)), unsafe.Sizeof(elevated),
		uintptr(unsafe.Pointer(&retLen)))
	return ret != 0 && elevated != 0
}
