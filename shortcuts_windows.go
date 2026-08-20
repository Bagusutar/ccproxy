//go:build windows

package main

import (
	"reflect"
	"unsafe"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
)

// disableBrowserShortcuts 关掉 WebView2 自带的整套浏览器快捷键
// （F1 帮助、Ctrl+R / F5 刷新、Ctrl+P 打印、Ctrl+F 查找、Ctrl+± 缩放……）。
//
// 为什么需要：这些键由 WebView2 宿主层处理，早于 DOM 事件，所以在 JS 里
// preventDefault 拦不住；而且那样做等于「捕获更多按键」，与目标相反。
// 关掉之后按键落到窗口无人认领，不会被本应用消费。
//
// 为什么用反射：go-webview2 在 NewWithOptions 内部持有 *edge.Chromium，
// 存放在非导出字段 browser 里，公开的 WebView 接口没有暴露 settings 的入口。
// 库只有这一个固定版本（v0.0.0-20260205173254），字段改名的风险接近于零；
// 相比之下 fork 整个库只为三行补丁并不划算。
//
// 取不到时静默返回：功能退化成「浏览器快捷键仍然生效」，不影响其他任何行为。
func disableBrowserShortcuts(w webview2.WebView) bool {
	defer func() { _ = recover() }()

	v := reflect.ValueOf(w)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return false
	}
	f := v.Elem().FieldByName("browser")
	if !f.IsValid() || !f.CanAddr() {
		return false
	}
	// browser 是非导出字段，用 NewAt 绕过可见性限制读取其接口值
	iface := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
	chromium, ok := iface.(*edge.Chromium)
	if !ok || chromium == nil {
		return false
	}
	settings, err := chromium.GetSettings()
	if err != nil || settings == nil {
		return false
	}
	if err := settings.PutAreBrowserAcceleratorKeysEnabled(false); err != nil {
		return false
	}
	// 顺带关掉缩放：Ctrl+± 与 Ctrl+滚轮在配置窗口里没有意义，
	// 误触后布局错乱反而像是 bug。
	_ = settings.PutIsZoomControlEnabled(false)
	return true
}
