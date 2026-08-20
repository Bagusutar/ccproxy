//go:build !windows

package main

import "os/exec"

// 非 Windows 平台没有控制台窗口问题，原样返回。
func hideConsole(cmd *exec.Cmd) *exec.Cmd { return cmd }
