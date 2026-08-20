package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

// 省略的 /v1 版本段要补回来，让同一个本地地址同时适用于
// Anthropic（base 不含 /v1）与 OpenAI（base 含 /v1）两种约定。
func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		// OpenAI SDK 的 base 已含 /v1，只发出后半段
		"/chat/completions": "/v1/chat/completions",
		"/responses":        "/v1/responses",
		"/embeddings":       "/v1/embeddings",
		"/messages":         "/v1/messages",
		"/models":           "/v1/models",

		// 已经带了版本段的原样不动
		"/v1/messages":         "/v1/messages",
		"/v1/chat/completions": "/v1/chat/completions",
		"/v1/models":           "/v1/models",
		"/v2/messages":         "/v2/messages",

		// 未知路径一律原样转发——代理不该猜测上游的路由
		"/foo":               "/foo",
		"/anthropic/v1/chat": "/anthropic/v1/chat",
		"/chat/completions/": "/chat/completions/",
		"":                   "",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// 档位后缀必须在转发前从请求体里剥掉并换成 beta 头：
// 实测裸名 + context-1m-2025-08-07 → 200，带后缀 → 403。
func TestStripModelSuffix(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantModel string
		wantBeta  string
		wantHit   bool
	}{
		{"1m 小写", `{"model":"claude-opus-5[1m]","max_tokens":1}`,
			"claude-opus-5", "context-1m-2025-08-07", true},
		{"1M 大写", `{"model":"claude-opus-5[1M]"}`,
			"claude-opus-5", "context-1m-2025-08-07", true},
		// 未知档位仍然剥掉：带后缀的名字在上游必然失败，
		// 但不能凭空编一个 beta id，那只会换来另一种 400。
		{"未知档位", `{"model":"claude-opus-5[9m]"}`, "claude-opus-5", "", true},
		{"无后缀不动", `{"model":"claude-opus-5"}`, "claude-opus-5", "", false},
		{"没有 model 字段", `{"max_tokens":1}`, "", "", false},
		// 后缀只在结尾才算，中间出现的方括号是模型名的一部分。
		{"中间的方括号", `{"model":"a[1m]b"}`, "a[1m]b", "", false},
	}
	for _, c := range cases {
		out, beta, hit := stripModelSuffix([]byte(c.in))
		if hit != c.wantHit {
			t.Errorf("%s: changed = %v, want %v", c.name, hit, c.wantHit)
		}
		if beta != c.wantBeta {
			t.Errorf("%s: beta = %q, want %q", c.name, beta, c.wantBeta)
		}
		if got := gjson.GetBytes(out, "model").String(); got != c.wantModel {
			t.Errorf("%s: model = %q, want %q", c.name, got, c.wantModel)
		}
		// 其余字段必须原样保留——改写只能碰 model。
		if c.wantHit && gjson.GetBytes([]byte(c.in), "max_tokens").Exists() {
			if !gjson.GetBytes(out, "max_tokens").Exists() {
				t.Errorf("%s: 改写把 max_tokens 弄丢了", c.name)
			}
		}
	}
}

// 追加而非覆盖：客户端可能已经声明了别的 beta 特性。
func TestAppendBeta(t *testing.T) {
	cases := []struct{ existing, add, want string }{
		{"", "context-1m-2025-08-07", "context-1m-2025-08-07"},
		{"tool-use-2024", "context-1m-2025-08-07", "tool-use-2024,context-1m-2025-08-07"},
		// 已经声明过就不重复加
		{"context-1m-2025-08-07", "context-1m-2025-08-07", "context-1m-2025-08-07"},
		{"a, context-1m-2025-08-07 ,b", "context-1m-2025-08-07", "a, context-1m-2025-08-07 ,b"},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.existing != "" {
			h.Set("Anthropic-Beta", c.existing)
		}
		appendBeta(h, c.add)
		if got := h.Get("Anthropic-Beta"); got != c.want {
			t.Errorf("appendBeta(%q, %q) = %q, want %q", c.existing, c.add, got, c.want)
		}
	}
}

// 用户手上的 exe 放在哪里都不该影响行为。服务映像固定落在 ccproxy 自己的
// 数据目录下（Windows 上是 %LOCALAPPDATA%\ccproxy），而不是 Claude Code 的
// 配置目录——那个目录属于 Claude Code，ccproxy 只读不写。
//
// 名字必须与用户那份不同。守护进程会一直占用服务映像，若两者同名，
// 用户看到的是「我的 exe 好像被复制了一份，还删不掉」——机制没错，
// 呈现全错。叫 ccproxy-service 就自解释了。
func TestServiceExeIsSeparateFromUserExe(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	appDir := sandboxDataDir(t)

	dst, err := InstallService()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(appDir, "ccproxy", "ccproxy-service")
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if dst != want {
		t.Fatalf("落点 = %q, want %q", dst, want)
	}
	// 不能落在 Claude Code 的配置目录里
	if cd, _ := claudeDir(); strings.HasPrefix(dst, cd) {
		t.Errorf("服务映像落进了 Claude Code 的配置目录: %q", dst)
	}
	if filepath.Base(dst) == filepath.Base(os.Args[0]) {
		t.Error("服务映像与用户 exe 同名，正是这次要消除的混淆")
	}
	src, _ := os.Executable()
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Errorf("复制的内容与源文件不一致 (%d vs %d 字节)", len(a), len(b))
	}

	// 从落点自身再运行一次不该出问题——升级时会走到这条。
	// Windows 上正在运行的 exe 不能被覆盖，所以这条短路是必须的。
	again, err := InstallService()
	if err != nil || again != dst {
		t.Fatalf("重复安装 = %q, %v", again, err)
	}
}

// 保存时应复用已记录的探测结论：模型多起来之后，每次保存都实调一遍
// 会变成几十秒的等待，而绝大多数保存跟模型能不能用毫无关系。
// 缓存在改地址或凭证时由界面清空，所以不存在「留着旧上游结论」的风险。
func TestVerifySlotsSkipsKnownModels(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[]}`)
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.Providers[0].BaseURL = upstream.URL
	cfg.Providers[0].Token = "t"
	cfg.Providers[0].ModelProtocols = map[string][]string{"known-model": {"anthropic"}}
	cfg.Slots["main"] = Slot{Provider: "p1", Model: "known-model"}

	if bad := verifySlots(cfg); len(bad) > 0 {
		t.Fatalf("已有结论的模型不该被判失败: %v", bad)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("已有结论却仍发了 %d 次请求", n)
	}

	// 没记录过的模型仍然要真测，否则路由拿不到方言表。
	cfg.Slots["opus"] = Slot{Provider: "p1", Model: "fresh-model"}
	if bad := verifySlots(cfg); len(bad) > 0 {
		t.Fatalf("未记录的模型应当被实测并通过: %v", bad)
	}
	if n := atomic.LoadInt32(&hits); n == 0 {
		t.Error("未记录的模型没有被实测")
	}
	if got := cfg.Providers[0].ModelProtocols["fresh-model"]; len(got) == 0 {
		t.Error("实测结论应当写回配置，供下次复用")
	}
}

// sandboxDataDir 把 ccproxy 自己的数据目录指到临时目录。
//
// 数据目录已经不跟 CLAUDE_CONFIG_DIR 走了：Windows 上看 LOCALAPPDATA，
// 其他平台走 os.UserConfigDir（即 XDG_CONFIG_HOME）。只设 CLAUDE_CONFIG_DIR
// 的测试会跑到开发机真实的 ~/.config/ccproxy 上去建文件、删目录——
// 这条踩过，所以凡是碰数据目录的测试都必须先调它。
func sandboxDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LOCALAPPDATA", dir)
	return dir
}

// 「只打开面板、从没保存过配置」必须零残留：读路径一个目录都不许建。
//
// 这条守的是一个真实故障：曾经读路径也走会创建的那个变体，于是
// 「卸载并还原」刚把目录删干净，界面 5 秒一次的状态轮询就把空目录建了回来，
// 用户看到的是「说是删了，回头一看还在」。
func TestReadPathsNeverCreateDataDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	app := sandboxDataDir(t)
	dir := filepath.Join(app, "ccproxy")

	// 面板启动与轮询会走到的全部读路径
	if _, err := LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ReadStatus()
	ReadMeter()
	if _, err := dataDirPath(); err != nil {
		t.Fatal(err)
	}
	if _, err := configPath(); err != nil {
		t.Fatal(err)
	}
	if _, err := logPath(); err != nil {
		t.Fatal(err)
	}
	if _, err := ServiceExePath(); err != nil {
		t.Fatal(err)
	}
	cleanupInstallLeftovers()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		ents, _ := os.ReadDir(dir)
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("读路径把数据目录建出来了（内容 %v）", names)
	}
}

func TestValidateConfigAcceptsSubagentSlot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Providers[0].BaseURL = "https://example.com"
	cfg.Providers[0].Token = "test"
	cfg.Slots["subagent"] = Slot{Provider: "p1", Model: "claude-haiku"}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("subagent slot rejected: %v", err)
	}
	for _, sd := range slotEnvKeys {
		if sd.Key == "subagent" {
			t.Fatal("subagent compatibility slot must not rewrite CLAUDE_CODE_SUBAGENT_MODEL")
		}
	}
}

func TestNormalizeSlotsRemovesEmptyKnownSlots(t *testing.T) {
	slots := normalizeSlots(map[string]Slot{
		"main":    {Provider: "p1", Model: " \t"},
		"opus":    {Provider: "p1", Model: "m"},
		"unknown": {Provider: "p1"},
	})
	if _, ok := slots["main"]; ok {
		t.Error("empty known slot was not removed")
	}
	if got := slots["opus"].Model; got != "m" {
		t.Errorf("assigned slot changed: %q", got)
	}
	if _, ok := slots["unknown"]; !ok {
		t.Error("normalization hid an unknown slot from validation")
	}
}

func TestConfigTimeoutBoundsLoadAndSave(t *testing.T) {
	sandboxDataDir(t)
	p, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"port":15722,"providers":[{"id":"p1"}],"slots":{},"firstByteSec":86401,"stallSec":60}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig accepted timeout above bound")
	}

	cfg := DefaultConfig()
	cfg.StallSec = -1
	if err := SaveConfig(cfg); err == nil {
		t.Fatal("SaveConfig accepted negative timeout")
	}
	cfg = DefaultConfig()
	cfg.FirstByteSec = maxTimeoutSeconds
	cfg.StallSec = maxTimeoutSeconds
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("maximum sensible timeout rejected: %v", err)
	}
}

func TestMigrationDefaultsOnlyOmittedTimeouts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FirstByteSec, cfg.StallSec = 0, 0
	cfg.migrate()
	if cfg.FirstByteSec != defaultFirstByte || cfg.StallSec != defaultStall {
		t.Fatalf("omitted timeouts did not get defaults: %+v", cfg)
	}
	cfg.FirstByteSec, cfg.StallSec = -1, -1
	cfg.migrate()
	if err := validateConfig(cfg); err == nil {
		t.Fatal("negative timeouts were silently defaulted")
	}
}

func TestLoadConfigRejectsInvalidPrices(t *testing.T) {
	sandboxDataDir(t)
	p, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"port":15722,"providers":[{"id":"p1"}],"slots":{},"firstByteSec":95,"stallSec":60,"prices":{"m":{"in":-1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig accepted invalid persisted price")
	}
}

func TestConfigValidationIDsSlotsAndPort(t *testing.T) {
	cfg := DefaultConfig()
	for _, port := range []int{-1, 0, 65536} {
		cfg.Port = port
		if err := validateConfig(cfg); err == nil {
			t.Errorf("port %d accepted", port)
		}
	}
	cfg = DefaultConfig()
	cfg.Providers = []Provider{{ID: "same"}, {ID: "same"}}
	if err := validateConfig(cfg); err == nil {
		t.Error("duplicate IDs accepted")
	}
	cfg.Providers = []Provider{{ID: "bad id"}}
	if err := validateConfig(cfg); err == nil {
		t.Error("invalid ID accepted")
	}
	cfg.Providers = []Provider{{ID: "p1"}}
	cfg.Slots = map[string]Slot{"main": {Provider: "missing", Model: "m"}}
	if err := validateConfig(cfg); err == nil {
		t.Error("dangling slot accepted")
	}
}

func TestMigrationRepairsProviderIDs(t *testing.T) {
	cfg := &Config{Port: defaultPort, FirstByteSec: defaultFirstByte, StallSec: defaultStall, Providers: []Provider{{ID: ""}, {ID: "p1"}, {ID: "p1"}}, Slots: map[string]Slot{"main": {Provider: "", Model: "m"}}}
	cfg.migrate()
	seen := map[string]bool{}
	for _, p := range cfg.Providers {
		if !providerIDPattern.MatchString(p.ID) || seen[p.ID] {
			t.Fatalf("invalid migrated IDs: %+v", cfg.Providers)
		}
		seen[p.ID] = true
	}
	if !seen[cfg.Slots["main"].Provider] {
		t.Fatalf("slot not remapped: %+v", cfg.Slots["main"])
	}
}

// 自启默认开启，但只在从未安装过的时候。
// 用户装完之后自己关掉的，任何默认值都不该把它打开回去。
func TestAutostartDefault(t *testing.T) {
	for _, c := range []struct {
		enabled, installed, want bool
		why                      string
	}{
		{false, false, true, "全新面板：默认开启"},
		{true, false, true, "系统里已启用：跟随"},
		{true, true, true, "装过且启用着：跟随"},
		{false, true, false, "装过但被用户关掉：必须保持关闭"},
	} {
		if got := autostartDefault(c.enabled, c.installed); got != c.want {
			t.Errorf("%s: autostartDefault(%v,%v) = %v, want %v",
				c.why, c.enabled, c.installed, got, c.want)
		}
	}
}
