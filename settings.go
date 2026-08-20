package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// managedKeys 是 ccproxy 唯一会改动的字段。除此之外的任何内容
// （permissions / hooks / theme / effortLevel / enabledPlugins ...）
// 一个字节都不会被触碰。
var managedKeys = func() []string {
	keys := []string{
		"env.ANTHROPIC_BASE_URL",
		"env.ANTHROPIC_AUTH_TOKEN",
		"env.CLAUDE_CODE_RETRY_WATCHDOG",
	}
	for _, s := range slotEnvKeys {
		keys = append(keys, "env."+s.Env)
	}
	return keys
}()

// backupSuffix 是备份文件名的固定部分，其后接时间戳。
// 卸载时靠这个前缀把备份找回来删掉，所以两处必须用同一个常量。
const backupSuffix = ".ccproxy-backup-"

// tokenPlaceholder 写入 settings.json 的占位符。真实凭证只存在
// ccproxy 的 config.json（0600）与内存中，不落到 Claude Code 配置里。
const tokenPlaceholder = "ccproxy-managed"

// SettingsFile 返回本次要操作的 settings.json 路径：
// 用户显式指定的优先，否则回退到系统默认位置。
func (c *Config) SettingsFile() string {
	if p := strings.TrimSpace(c.SettingsPath); p != "" {
		return p
	}
	p, _ := ClaudeSettingsPath()
	return p
}

// ClaudeSettingsPath 返回用户级 settings.json 的默认路径。
// Windows: %USERPROFILE%\.claude\settings.json
func ClaudeSettingsPath() (string, error) {
	dir, err := claudeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// SettingsSnapshot 是安装前对 settings.json 的观察结果。
type SettingsSnapshot struct {
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	Valid      bool   `json:"valid"`
	ParseError string `json:"parseError,omitempty"`
	BaseURL    string `json:"baseUrl"`
	AuthToken  string `json:"authToken"`
	Model      string `json:"model"`
	ManagedNow bool   `json:"managedNow"` // 当前是否已指向 ccproxy
	Raw        string `json:"-"`
}

// InspectSettings 读取并解析指定的 settings.json，不做任何修改。
// path 留空时使用默认路径。
func InspectSettings(path string) (*SettingsSnapshot, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		var err error
		if p, err = ClaudeSettingsPath(); err != nil {
			return nil, err
		}
	}
	snap := &SettingsSnapshot{Path: p}

	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		snap.Exists = false
		snap.Valid = true // 不存在时视为可安全创建
		return snap, nil
	}
	if err != nil {
		return nil, err
	}
	snap.Exists = true
	snap.Raw = string(raw)

	if !gjson.ValidBytes(raw) {
		snap.Valid = false
		snap.ParseError = "settings.json 不是合法 JSON"
		return snap, nil
	}
	snap.Valid = true
	snap.BaseURL = gjson.GetBytes(raw, "env.ANTHROPIC_BASE_URL").String()
	snap.AuthToken = firstNonEmpty(
		gjson.GetBytes(raw, "env.ANTHROPIC_AUTH_TOKEN").String(),
		gjson.GetBytes(raw, "env.ANTHROPIC_API_KEY").String(),
	)
	snap.Model = gjson.GetBytes(raw, "env.ANTHROPIC_MODEL").String()
	snap.ManagedNow = isLocalProxyURL(snap.BaseURL) && snap.AuthToken == tokenPlaceholder
	return snap, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isLocalProxyURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || port == "" {
		return false
	}
	p, err := strconv.Atoi(port)
	if err != nil || !validPort(p) {
		return false
	}
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1"
}

// ---------- 输出格式 ----------

// detectIndent 沿用文件原有的缩进宽度，避免把用户的 4 空格改成 2 空格。
// 找不到任何缩进行（空文件、单行 JSON）时用 2 空格。
func detectIndent(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// sjsonKey 转义 sjson 路径里的元字符。settings.json 的顶层键目前都不含它们，
// 但重排时是把任意已有键原样搬运，不该假设。
var sjsonKeyEscaper = strings.NewReplacer(`\`, `\\`, `.`, `\.`, `*`, `\*`, `?`, `\?`)

// formatSettings 按 indent 整体格式化，必要时把 env 提到首位。
//
// sjson 是纯文本定点编辑，新建的键一律追加到末尾且不换行——我们凭空创建
// env 时，它会变成挤在文件最后的一长行。hoistEnv 就是为这一种情况准备的：
// 重排后再 json.Indent（只做文本变换、不解析成 map，其余键的顺序原样保留）。
//
// env 本来就存在时绝不重排。那时 sjson 是就地改值，位置本就没动，
// 再去搬一次就是无缘无故打乱用户文件的键序——而界面承诺的是
// 「其余内容含键顺序保持不变」，这句话得是真的。
func formatSettings(raw []byte, indent string, hoistEnv bool) ([]byte, error) {
	if env := gjson.GetBytes(raw, "env"); hoistEnv && env.Exists() {
		rebuilt, err := sjson.SetRawBytes([]byte("{}"), "env", []byte(env.Raw))
		if err != nil {
			return nil, err
		}
		gjson.ParseBytes(raw).ForEach(func(k, v gjson.Result) bool {
			key := k.String()
			if key == "env" {
				return true
			}
			rebuilt, err = sjson.SetRawBytes(rebuilt, sjsonKeyEscaper.Replace(key), []byte(v.Raw))
			return err == nil
		})
		if err != nil {
			return nil, err
		}
		raw = rebuilt
	}

	var buf bytes.Buffer
	// 先剪掉首尾空白再缩进：json.Indent 会把 src 末尾的空白原样带过去，
	// 而我们随后又固定补一个换行——不剪的话每保存一次就在用户文件末尾
	// 多一个空行。以前 env 重排顺手用 sjson 重建了整个对象，把这件事盖住了。
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", indent); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// ccproxyOwnValue 判断某个受管键当前的值是不是 ccproxy 自己写进去的。
//
// 防的是一种自噬状态：config.json 没了（换机器、手工清理、数据目录被清空），
// 而 settings.json 还指着代理。此时 cfg.Installed 为 false，ApplySettings 会
// 把「ccproxy 写的值」当成用户原值记进 Original，还原时再原样写回去——
// 于是 BASE_URL 永远停在 http://127.0.0.1:PORT、AUTH_TOKEN 永远是占位符，
// 用户再也回不到真实上游，而界面还报告「已还原」。
//
// 实测踩过：卸载后 settings.json 里 BASE_URL 仍是 127.0.0.1:15722、
// AUTH_TOKEN 仍是 ccproxy-managed，六个模型位倒是删干净了。
//
// 认不出用户原值时，删键优于写回一个已知错误的值：删掉至少回到
// 「没配过」这个干净状态，Claude Code 会走自己的默认；写回去则是永久损坏。
func ccproxyOwnValue(key, val string) bool {
	switch key {
	case "env.ANTHROPIC_BASE_URL":
		return isLocalProxyURL(val)
	case "env.ANTHROPIC_AUTH_TOKEN":
		return val == tokenPlaceholder
	}
	return false
}

// recordOriginal 记下每个受管键在 ccproxy 动手之前的值。
// nil 表示该键原本不存在，还原时应删除而非置空。
func recordOriginal(cfg *Config, raw string, managedNow bool) {
	cfg.Original = map[string]*json.RawMessage{}
	for _, k := range managedKeys {
		r := gjson.Get(raw, k)
		// 文件此刻已经被 ccproxy 接管时，里面每一个受管键的值都是我们自己
		// 写的，没有一个是用户的原值——一律记成「原本不存在」。
		if !r.Exists() || managedNow || ccproxyOwnValue(k, r.String()) {
			cfg.Original[k] = nil
			continue
		}
		v := json.RawMessage(append([]byte(nil), r.Raw...))
		cfg.Original[k] = &v
	}
}

// ApplySettings 把 settings.json 指向本地代理。
//
// 语义是「定点替换」而非「重写」：只改 managedKeys 涉及的路径，
// 其余键及其顺序、值原样保留；最后统一按文件原有缩进排版，
// 并把 env 块提到最前面，便于人肉核对。
// 每个被改键的原值记入 cfg.Original，供 RestoreSettings 精确还原。
func ApplySettings(cfg *Config) (backupPath string, err error) {
	snap, err := InspectSettings(cfg.SettingsPath)
	if err != nil {
		return "", err
	}
	if snap.Exists && !snap.Valid {
		// 最重要的安全阀：宁可什么都不做，也不能覆盖一个我们读不懂的文件。
		return "", fmt.Errorf("拒绝修改：%s", snap.ParseError)
	}

	raw := snap.Raw
	if !snap.Exists || len(raw) == 0 {
		raw = "{}"
	}
	// 记在写入之前：env 是我们凭空建的，才需要把它从文件末尾提上来。
	envWasAbsent := !gjson.Get(raw, "env").Exists()

	// 备份：只在 ccproxy 第一次动这个文件之前做一次。
	//
	// 每次保存都备份看着更稳妥，实际备的是 ccproxy 自己上一次写进去的内容——
	// 而还原走 cfg.Original 逐键回滚，根本不读备份文件。真正有价值的只有
	// 「ccproxy 动手之前」那一份，多出来的只会在用户的 .claude 目录里
	// 按保存次数累积（实测三次保存就是三个备份）。
	//
	// 判据与记录 cfg.Original 是同一个：两者保护的是同一样东西。
	if snap.Exists && !cfg.Installed {
		backupPath = snap.Path + backupSuffix + time.Now().Format("20060102-150405")
		if err := atomicWrite(backupPath, []byte(raw), 0o600); err != nil {
			return "", fmt.Errorf("备份失败: %w", err)
		}
		cfg.BackupPath = backupPath
	}

	// 首次安装时，把当前的上游配置抽出来，用户无需手填。
	if !cfg.Installed {
		if dp := cfg.DefaultProvider(); dp != nil {
			if snap.BaseURL != "" && !snap.ManagedNow && dp.BaseURL == "" {
				dp.BaseURL = snap.BaseURL
			}
			if snap.AuthToken != "" && snap.AuthToken != tokenPlaceholder && dp.Token == "" {
				dp.Token = snap.AuthToken
			}
		}
		// 记录原值：nil 表示该键原本不存在，还原时应删除而非置空。
		recordOriginal(cfg, raw, snap.ManagedNow)
	}

	// 删除当前已清空的可选键，避免 set→clear 后残留旧槽位或 watchdog。
	out := raw
	for _, sd := range slotEnvKeys {
		slot, ok := cfg.Slots[sd.Key]
		if ok && strings.TrimSpace(slot.Model) != "" {
			continue
		}
		key := "env." + sd.Env
		out, err = sjson.Delete(out, key)
		if err != nil {
			return "", fmt.Errorf("清理 %s 失败: %w", key, err)
		}
	}
	if !cfg.RetryWatchdog {
		const key = "env.CLAUDE_CODE_RETRY_WATCHDOG"
		out, err = sjson.Delete(out, key)
		if err != nil {
			return "", fmt.Errorf("清理 %s 失败: %w", key, err)
		}
	}

	// 逐键定点写入。用有序切片而非 map：map 遍历顺序随机，
	// 每次保存都会把 env 里的键洗牌一遍，diff 全是噪声。
	type kv struct{ k, v string }
	writes := []kv{
		{"env.ANTHROPIC_BASE_URL", fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)},
		{"env.ANTHROPIC_AUTH_TOKEN", tokenPlaceholder},
	}
	// 每个模型位：写入裸模型名，1M 开关打开时附加 [1M] 后缀。
	// 该后缀由 Claude Code 客户端解析成 context-1m beta 头，不会发给上游。
	for _, sd := range slotEnvKeys {
		slot, ok := cfg.Slots[sd.Key]
		if !ok || strings.TrimSpace(slot.Model) == "" {
			continue
		}
		name := strings.TrimSpace(slot.Model)
		if slot.OneM {
			name += "[1M]"
		}
		writes = append(writes, kv{"env." + sd.Env, name})
	}
	if cfg.RetryWatchdog {
		writes = append(writes, kv{"env.CLAUDE_CODE_RETRY_WATCHDOG", "1"})
	}

	for _, w := range writes {
		out, err = sjson.Set(out, w.k, w.v)
		if err != nil {
			return "", fmt.Errorf("写入 %s 失败: %w", w.k, err)
		}
	}

	formatted, err := formatSettings([]byte(out), detectIndent(raw), envWasAbsent)
	if err != nil {
		return "", fmt.Errorf("格式化 settings.json 失败: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(snap.Path), 0o700); err != nil {
		return "", err
	}
	if err := atomicWrite(snap.Path, formatted, 0o600); err != nil {
		return "", err
	}

	cfg.Installed = true
	cfg.InstalledSettingsPath = snap.Path
	cfg.InstalledAt = time.Now().Format(time.RFC3339)
	return backupPath, nil
}

// RestoreSettings 精确还原：对每个受管键，原本存在则写回原值，
// 原本不存在则删除该键。不整体覆盖文件，因此用户在安装之后
// 对 settings.json 做的其他修改都会保留。
func RestoreSettings(cfg *Config) error {
	path := cfg.InstalledSettingsPath
	if strings.TrimSpace(path) == "" {
		path = cfg.SettingsPath // legacy configs
	}
	snap, err := InspectSettings(path)
	if err != nil {
		return err
	}
	if !snap.Exists {
		return nil
	}
	if !snap.Valid {
		return fmt.Errorf("拒绝修改：%s", snap.ParseError)
	}

	out := snap.Raw
	configLost := !cfg.Installed && snap.ManagedNow && len(cfg.Original) == 0
	for _, k := range managedKeys {
		orig, tracked := cfg.Original[k]
		if configLost {
			tracked, orig = true, nil
		}
		switch {
		case !tracked:
			// 未记录过的键不动。
			continue
		case orig == nil:
			out, err = sjson.Delete(out, k)
		case ccproxyOwnValue(k, gjson.ParseBytes(*orig).String()):
			// 记录本身就是坏的（见 ccproxyOwnValue）。写回去等于把一个指向
			// 本机代理的死地址和一个占位符凭证永久留在用户文件里。
			// 这一条也负责修好早期版本已经记坏的配置。
			out, err = sjson.Delete(out, k)
		default:
			out, err = sjson.SetRaw(out, k, string(*orig))
		}
		if err != nil {
			return fmt.Errorf("还原 %s 失败: %w", k, err)
		}
	}

	// 我们创建的 env 块被掏空后不该留下一个空壳。
	if env := gjson.Get(out, "env"); env.Exists() && env.IsObject() && len(env.Map()) == 0 {
		if out, err = sjson.Delete(out, "env"); err != nil {
			return fmt.Errorf("清理空 env 失败: %w", err)
		}
	}

	// 还原只删键、不建键，没有「新键挤在末尾」这个问题，所以不重排。
	formatted, err := formatSettings([]byte(out), detectIndent(snap.Raw), false)
	if err != nil {
		return fmt.Errorf("格式化 settings.json 失败: %w", err)
	}
	if err := atomicWrite(snap.Path, formatted, 0o600); err != nil {
		return err
	}
	cfg.Installed = false
	cfg.InstalledSettingsPath = ""
	return nil
}

// PurgeFootprint 删掉 ccproxy 在本机创建过的每一个文件。
//
// 「还原」的语义是让这台机器回到没装过的样子，所以清理范围不止配置：
//
//	<settings.json 同级>       settings.json.ccproxy-backup-*
//	%LOCALAPPDATA%\ccproxy\    config.json（含真实凭证）、status.json、ccproxy.log、
//	                           usage.json、ccproxy-service.exe 及其 .old、
//	                           .ccproxy-tmp-*，清空后连目录一起删
//
// Claude Code 自己的配置目录不在范围内：ccproxy 对它只读不写，
// 唯一动过的就是那份 settings.json，而它由 RestoreSettings 逐键还原。
//
// WebView2 的用户数据目录也不在范围内——它在系统临时目录下，
// 由面板退出时的 defer 收掉，见 gui_windows.go。
//
// 用户手上那个 ccproxy.exe 同样不在范围内：它放在哪由用户决定，
// ccproxy 从不记住它的路径，也就没有资格去删它。
//
// 唯一可能删不掉的是「面板恰好就是从数据目录里运行的」这种情形——
// Windows 不允许删除已加载的映像。正常用法下面板在别处，不会发生；
// 真发生时它的路径原样返回，由界面如实告知用户手动删除。
//
// 刻意不派一个后台进程去延时删自己：那个套路在杀软眼里与恶意软件同形，
// 而本程序已经在写 Run 键、改 settings.json，不该再叠一层可疑行为；
// 何况它失败时是静默的，用户以为干净了，其实没有。少一个文件的收益，
// 换不来「行为可解释」这件事。
//
// 返回 dir 供界面提示位置；leftover 为空即表示已彻底清干净。
func PurgeFootprint(settingsFile string) (dir string, leftover []string, err error) {
	self := ""
	if exe, e := os.Executable(); e == nil {
		self, _ = filepath.Abs(exe)
	}
	return purgeFootprint(settingsFile, self)
}

// purgeFootprint 是 PurgeFootprint 的可测形态：把「哪个文件删不掉」这个
// 唯一的外部事实作为参数传进来。测试进程本身不在数据目录里跑，
// 不注入的话「保留正在运行的 exe」这条分支永远走不到。
func purgeFootprint(settingsFile, self string) (dir string, leftover []string, err error) {
	var firstErr error
	note := func(e error) {
		if e != nil && firstErr == nil && !errors.Is(e, os.ErrNotExist) {
			firstErr = e
		}
	}

	// 1. settings.json 的备份，文件名是「原名 + 后缀 + 时间戳」。
	//
	// 用读目录 + 前缀匹配，而不是 filepath.Glob：Windows 上 Glob 不支持转义
	// （反斜杠在那里是路径分隔符），而这个路径由用户填写，含 [ ] 的目录名
	// 会让模式静默失配——备份删不掉，还不报错。
	if s := strings.TrimSpace(settingsFile); s != "" {
		parent, prefix := filepath.Dir(s), filepath.Base(s)+backupSuffix
		ents, _ := os.ReadDir(parent)
		for _, ent := range ents {
			if strings.HasPrefix(ent.Name(), prefix) {
				note(os.Remove(filepath.Join(parent, ent.Name())))
			}
		}
	}

	// 2. 数据目录里的一切。
	dir, err = dataDirPath()
	if err != nil {
		return "", nil, err
	}

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return dir, nil, firstErr
	}
	if err != nil {
		return dir, nil, err
	}
	for _, ent := range entries {
		p := filepath.Join(dir, ent.Name())
		// 正在运行的自己删不掉，别去试——白白耗掉整轮重试。
		// Windows 的路径比较不分大小写；Linux 上 EqualFold 也不会误判，
		// 因为两边都是同一个 os.Executable() 推导出来的绝对路径。
		if self != "" && strings.EqualFold(p, self) {
			continue
		}
		note(removeWithRetry(p))
	}

	// leftover 一律以「目录里现在真的还剩什么」为准，而不是「我打算跳过什么」。
	// 前者是事实，后者是意图——一旦某个删除失败，两者就分叉，
	// 而用户看到的会是后者。实测卸载时 ccproxy.log 没删掉，
	// 对话框却照样说「只剩程序本身」，就是这么来的。
	// 从最终扫描得出，任何删不掉的东西都会自动如实出现，不必预先枚举失败原因。
	if os.Remove(dir) != nil {
		rest, _ := os.ReadDir(dir)
		for _, r := range rest {
			leftover = append(leftover, filepath.Join(dir, r.Name()))
		}
	}
	return dir, leftover, firstErr
}

// removeWithRetry 删不掉就等一下再试。
//
// 卸载紧跟在 taskkill 之后，而 Windows 上「进程被结束」到「句柄真正释放」
// 之间有几百毫秒空档——实测这段时间里 ccproxy.log（daemon 全程开着）和
// .ccproxy-tmp-*（心跳写到一半被杀）都还锁着，报
// "The process cannot access the file because it is being used by another process"。
// 杀毒软件正在扫描的文件同理。
//
// 与 removeWebView2Profile 用同一套办法，理由也相同。绝大多数文件第一次
// 就删掉了，只有真被占用的才会走进重试。
func removeWithRetry(path string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}
