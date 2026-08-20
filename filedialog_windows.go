//go:build windows

package main

import (
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	comdlg32            = syscall.NewLazyDLL("comdlg32.dll")
	procGetOpenFileName = comdlg32.NewProc("GetOpenFileNameW")
)

const (
	ofnFileMustExist   = 0x00001000
	ofnPathMustExist   = 0x00000800
	ofnHideReadOnly    = 0x00000004
	ofnNoChangeDir     = 0x00000008
	ofnExplorer        = 0x00080000
	ofnDontAddToRecent = 0x02000000
)

// openFileName 对应 Win32 的 OPENFILENAMEW。
// 字段顺序与填充必须与 C 结构一致，否则对话框会直接失败或崩溃。
type openFileName struct {
	StructSize      uint32
	_               uint32 // 64 位下的对齐填充
	Owner           uintptr
	Instance        uintptr
	Filter          *uint16
	CustomFilter    *uint16
	MaxCustomFilter uint32
	FilterIndex     uint32
	File            *uint16
	MaxFile         uint32
	_               uint32
	FileTitle       *uint16
	MaxFileTitle    uint32
	_               uint32
	InitialDir      *uint16
	Title           *uint16
	Flags           uint32
	FileOffset      uint16
	FileExtension   uint16
	DefExt          *uint16
	CustData        uintptr
	FnHook          uintptr
	TemplateName    *uintptr
	PvReserved      uintptr
	DwReserved      uint32
	FlagsEx         uint32
}

// PickSettingsFile 打开系统文件选择框，返回选中的绝对路径；取消时返回空串。
//
// WSL 里的配置也能选到——资源管理器把它挂在 \\wsl.localhost\<发行版>\home\... 下，
// 对话框可以直接导航过去，所以不需要额外做路径转换。
func PickSettingsFile(hwnd uintptr, initial string) string {
	// 模态对话框依赖线程消息队列，必须锁定在同一个 OS 线程上。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	buf := make([]uint16, 4096)
	if initial != "" {
		if u, err := syscall.UTF16FromString(initial); err == nil && len(u) < len(buf) {
			copy(buf, u)
		}
	}

	// 过滤器是双 NUL 结尾的「描述\x00通配符\x00…\x00」序列。
	filter := utf16Pairs(
		"Claude Code 配置 (settings.json)", "settings.json",
		"JSON 文件 (*.json)", "*.json",
		"所有文件 (*.*)", "*.*",
	)

	var initDir *uint16
	if d := filepath.Dir(initial); d != "" && d != "." {
		initDir, _ = syscall.UTF16PtrFromString(d)
	}
	title, _ := syscall.UTF16PtrFromString("选择 Claude Code 的 settings.json")

	ofn := openFileName{
		Owner:       hwnd,
		Filter:      &filter[0],
		FilterIndex: 1,
		File:        &buf[0],
		MaxFile:     uint32(len(buf)),
		InitialDir:  initDir,
		Title:       title,
		Flags: ofnFileMustExist | ofnPathMustExist | ofnHideReadOnly |
			ofnNoChangeDir | ofnExplorer | ofnDontAddToRecent,
	}
	ofn.StructSize = uint32(unsafe.Sizeof(ofn))

	ret, _, _ := procGetOpenFileName.Call(uintptr(unsafe.Pointer(&ofn)))
	if ret == 0 {
		return "" // 用户取消，或对话框失败
	}
	return syscall.UTF16ToString(buf)
}

// utf16Pairs 把「描述, 通配符」序列拼成 Win32 要求的双 NUL 结尾过滤器。
func utf16Pairs(parts ...string) []uint16 {
	var out []uint16
	for _, s := range parts {
		u, err := syscall.UTF16FromString(s)
		if err != nil {
			continue
		}
		out = append(out, u...) // UTF16FromString 已含结尾 NUL
	}
	return append(out, 0)
}
