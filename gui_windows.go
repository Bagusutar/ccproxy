//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jchv/go-webview2"
)

// webviewProfileDir 是 WebView2 用户数据目录在系统临时目录下的名字。
//
// 带 ccproxy- 前缀是必需的：临时目录是全系统共用的，一个叫 webview2 的
// 文件夹既看不出是谁建的，也可能与别的程序撞名。
const webviewProfileDir = "ccproxy-webview2"

// logShortcutResult 只在关闭浏览器快捷键失败时记一笔。
//
// 成功是常态，记下来只是噪声；更要紧的是写日志会顺带建出数据目录，
// 于是「打开面板看一眼就关掉、从没保存过配置」也会在 .claude 下留痕。
// 这条日志唯一的用途是回答「为什么我的全局热键被吃了」，
// 而那只在失败时才需要回答。
//
// GUI 以 windowsgui 子系统构建，没有 stdout/stderr，只能落文件。
func logShortcutResult(ok bool) {
	if ok {
		return
	}
	p, err := logPath()
	if err != nil {
		return
	}
	f, err := openLogFile(p)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s gui: disable browser accelerator keys failed"+
		" (browser shortcuts remain active)\n",
		time.Now().Format("2006/01/02 15:04:05.000000"))
}

// runGUI 打开原生 WebView2 窗口。
// Win11 自带 WebView2 Runtime，无需任何额外安装。
func runGUI() error {
	// 只允许一个控制面板：第二次双击会把已有窗口提到前台然后退出。
	release, ok := acquireGUISingleInstance()
	if !ok {
		return nil
	}
	defer release()

	srv, err := NewUIServer()
	if err != nil {
		return err
	}
	go func() { _ = srv.Serve() }()

	// WebView2 必须有一个用户数据目录，关不掉。里面全是可再生的 Chromium
	// 缓存（实测 14 MB / 202 个文件），ccproxy 自己没有任何状态存在其中——
	// 配置在 config.json 里。
	//
	// 放系统临时目录，而不是 .claude\ccproxy\ 下：那是操作系统专门给这类
	// 可丢弃数据准备的位置，进程正常退出时由下面的 defer 收掉，
	// 万一崩溃或被强杀，也轮得到系统的临时文件清理去兜底。
	// 放在数据目录下则相反——ccproxy 静息时那里应该只有配置和日志。
	dataPath := filepath.Join(os.TempDir(), webviewProfileDir)
	// 顺序要紧：先清掉上次崩溃或被强杀留下的残余，再建目录。
	// 反过来的话，刚建好的目录会被紧接着的清理删掉，等于没建。
	// 单实例保护已确保此刻没有别的面板在用它，清理是安全的。
	removeWebView2Profile(dataPath)
	// WebView2 拿到一个不存在的 DataPath 能否自己建出来，没有在目标环境上
	// 验证过，而赌错的代价是面板整个打不开。先建好，不依赖未经验证的外部行为。
	if err := os.MkdirAll(dataPath, 0o700); err != nil {
		dataPath = "" // 建不出来就交给 WebView2 自己选位置，至少面板能开
	}
	if dataPath != "" {
		// 先 defer 清理、后 defer Destroy：defer 是后进先出，这个顺序才能
		// 保证 WebView2 已经关闭再动它的文件。
		defer removeWebView2Profile(dataPath)
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:    false,
		DataPath: dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  "ccproxy",
			Width:  960,
			Height: 760,
			Center: true,
		},
	})
	if w == nil {
		return fmt.Errorf("WebView2 初始化失败,请确认系统已安装 WebView2 Runtime")
	}
	defer w.Destroy()

	hwnd := uintptr(w.Window())

	// 关掉 WebView2 自带的浏览器快捷键（F1/Ctrl+R/Ctrl+P/…），
	// 避免这个配置窗口在获得焦点时吃掉用户的全局热键。
	logShortcutResult(disableBrowserShortcuts(w))

	// 去掉系统标题栏，内容铺满整个窗口。
	applyWindowChrome(hwnd)
	applyWindowIcon(hwnd)

	// 界面回调窗口命令。必须 Dispatch 到 UI 线程执行：
	// ReleaseCapture 只对持有捕获的那个线程有效，跨线程调用会静默失败，
	// 表现就是拖拽完全没反应。
	_ = w.Bind("winctl", func(action string) {
		w.Dispatch(func() { windowCommand(hwnd, action) })
	})

	// 选择 settings.json 的原生对话框
	_ = w.Bind("pickfile", func(initial string) string {
		return PickSettingsFile(hwnd, initial)
	})

	go func() {
		<-srv.Quit()
		w.Dispatch(func() { w.Terminate() })
	}()

	w.Navigate(srv.URL())
	w.Run()
	return nil
}
