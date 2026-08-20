//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// 开机自启走 HKCU 的 Run 键。
//
// 原先用的是 `schtasks /SC ONLOGON`，但登录触发器属于机器范围，
// 标准用户无权创建，一律返回 Access is denied——`/RL LIMITED`
// 只约束任务运行时的权限，约束不了注册时需要的权限。
// 这个程序不该、也不会要求管理员权限，所以计划任务这条路走不通。
//
// 免提权的替代只有 HKCU\...\Run 和启动文件夹两种。后者要么用 COM
// 生成 .lnk（多一份依赖和不少样板代码），要么放 .bat（每次登录闪一下
// 控制台窗口）。Run 键没有这两个问题，且查询与删除都是原子的。
//
// 这是 ccproxy 唯一写在 .claude 目录之外的东西。绿色版容不下第二处，
// 所以原先那条「Run 键被封时提权注册计划任务」的兜底路径已经删除——
// 计划任务留在任务计划库里，是真正难清理的一处，而它要解决的场景
// （组策略封 Run 键）至今没有实际发生过。
const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "ccproxy"
)

func enableAutostartOS(exePath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()
	// 路径可能含空格，必须带引号，否则参数会被截断。
	if err := k.SetStringValue(runValueName, fmt.Sprintf(`"%s" --daemon`, exePath)); err != nil {
		return fmt.Errorf("写入自启项失败: %w", err)
	}
	return nil
}

func disableAutostartOS() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return nil // Run 键不存在，等同于未启用
	}
	defer k.Close()
	if err := k.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除自启项失败: %w", err)
	}
	return nil
}

func isAutostartEnabledOS() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValueName)
	return err == nil && v != ""
}
