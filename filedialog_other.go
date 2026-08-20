//go:build !windows

package main

// PickSettingsFile 仅在 Windows 上提供原生文件选择框。
// 其他平台返回空串，界面会退回手工输入路径。
func PickSettingsFile(hwnd uintptr, initial string) string { return "" }
