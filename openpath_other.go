//go:build !windows

package main

import "os/exec"

// openInFileManager 在系统文件管理器中打开目录。
func openInFileManager(path string) error {
	return exec.Command("xdg-open", path).Start()
}
