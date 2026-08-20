//go:build !windows

package main

import (
	"fmt"
	"os/exec"
)

// runGUI 在非 Windows 平台退化为「起服务 + 打开默认浏览器」。
// 开发调试与 WSL 用户都走这条路径。
func runGUI() error {
	srv, err := NewUIServer()
	if err != nil {
		return err
	}
	go func() { _ = srv.Serve() }()

	url := srv.URL()
	fmt.Println("ccproxy 配置界面:", url)
	// xdg-open 在 WSL 下通常由 wslu 转交 Windows 浏览器；失败也不影响使用。
	_ = exec.Command("xdg-open", url).Start()

	<-srv.Quit()
	return nil
}
