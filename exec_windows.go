//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW：从 GUI 进程启动控制台程序时，若不加这个标志，
// 每次调用都会闪出一个黑色控制台窗口。
// 当前的调用点是 taskkill（结束后台代理）与 SpawnDaemon（拉起后台代理）。
const createNoWindow = 0x08000000

func hideConsole(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd
}
