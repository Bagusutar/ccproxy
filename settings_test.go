package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 一份「脏」的真实感配置：键顺序随意、缩进不统一、含大量与我们无关的字段。
const messySettings = `{
  "permissions": {
    "defaultMode": "auto",
    "allow": ["Bash(git status)", "Read(**)"]
  },
  "theme": "light",
  "env": {
    "ANTHROPIC_BASE_URL": "https://gateway.example.com",
    "ANTHROPIC_AUTH_TOKEN": "sk-ORIGINAL-TOKEN",
    "ANTHROPIC_MODEL": "claude-opus-5[1M]",
    "SOME_UNRELATED_VAR": "keep-me"
  },
  "includeCoAuthoredBy": false,
      "effortLevel": "medium",
  "enabledPlugins": { "context7@claude-plugins-official": false },
  "statusLine": { "type": "command", "command": "~/bin/statusline.sh" }
}`

func setupFixture(t *testing.T, content string) (settingsDir string) {
	t.Helper()
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	sandboxDataDir(t)
	return cfgDir
}

func readSettings(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 核心需求：只改受管字段，其余一个字节都不动。
func TestApplyPreservesEverythingElse(t *testing.T) {
	dir := setupFixture(t, messySettings)

	cfg := DefaultConfig()
	cfg.Port = 15722
	cfg.RetryWatchdog = true
	cfg.Slots = map[string]Slot{
		"main": {Provider: "p1", Model: "claude-opus-5", OneM: true},
	}

	if _, err := ApplySettings(cfg); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	got := readSettings(t, dir)

	// 1. 受管字段已改写
	if v := gjson.Get(got, "env.ANTHROPIC_BASE_URL").String(); v != "http://127.0.0.1:15722" {
		t.Errorf("BASE_URL = %q", v)
	}
	if v := gjson.Get(got, "env.ANTHROPIC_AUTH_TOKEN").String(); v != tokenPlaceholder {
		t.Errorf("AUTH_TOKEN = %q，真实凭证不应留在 settings.json", v)
	}
	// 1M 开关打开时附加后缀
	if v := gjson.Get(got, "env.ANTHROPIC_MODEL").String(); v != "claude-opus-5[1M]" {
		t.Errorf("ANTHROPIC_MODEL = %q，1M 开关未生效", v)
	}

	// 2. 原有上游配置被抽取到 ccproxy 自己的配置里，用户无需手填
	dp := cfg.DefaultProvider()
	if dp == nil || dp.BaseURL != "https://gateway.example.com" {
		t.Errorf("未抽取上游地址: %+v", dp)
	}
	if dp != nil && dp.Token != "sk-ORIGINAL-TOKEN" {
		t.Errorf("未抽取上游凭证")
	}

	// 3. 无关字段必须逐一原样保留
	for path, want := range map[string]string{
		"permissions.defaultMode": "auto",
		"permissions.allow.0":     "Bash(git status)",
		"theme":                   "light",
		"env.SOME_UNRELATED_VAR":  "keep-me",
		"effortLevel":             "medium",
		"statusLine.command":      "~/bin/statusline.sh",
	} {
		if v := gjson.Get(got, path).String(); v != want {
			t.Errorf("字段 %s 被破坏: got %q want %q", path, v, want)
		}
	}
	if gjson.Get(got, "includeCoAuthoredBy").Bool() != false {
		t.Error("includeCoAuthoredBy 被破坏")
	}
	if gjson.Get(got, "enabledPlugins.context7@claude-plugins-official").Bool() != false {
		t.Error("enabledPlugins 被破坏")
	}

	// 4. 键顺序完全不动。env 本来就在，sjson 是就地改值，
	//    没有任何理由去搬它——界面承诺的就是「含键顺序保持不变」。
	wantOrder := []string{"permissions", "theme", "env", "includeCoAuthoredBy", "effortLevel", "enabledPlugins", "statusLine"}
	var gotOrder []string
	gjson.Parse(got).ForEach(func(k, _ gjson.Result) bool {
		gotOrder = append(gotOrder, k.String())
		return true
	})
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("键顺序不符:\n got %v\nwant %v", gotOrder, wantOrder)
	}

	// 5. env 块必须是格式化的，不能是挤成一行的长串
	if !strings.Contains(got, "\n  \"env\": {\n    \"ANTHROPIC_BASE_URL\"") {
		t.Errorf("env 块未按缩进格式化:\n%s", firstLines(got, 6))
	}
	// 6. 幂等：再保存一次必须逐字节相同。
	//    这是「键序稳定」真正要保证的东西——writes 若用 map，
	//    每次遍历顺序不同，新键的追加位置就会变，diff 全是噪声。
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatalf("第二次 ApplySettings: %v", err)
	}
	if again := readSettings(t, dir); again != got {
		t.Errorf("重复保存产生了差异，输出不稳定:\n第一次:\n%s\n第二次:\n%s",
			firstLines(got, 12), firstLines(again, 12))
	}

	// 7. 生成了备份
	if cfg.BackupPath == "" {
		t.Fatal("未生成备份")
	}
	if _, err := os.Stat(cfg.BackupPath); err != nil {
		t.Errorf("备份文件不存在: %v", err)
	}
}

// 还原必须精确：原本有的写回原值，原本没有的删掉。
func TestRestoreIsSurgical(t *testing.T) {
	dir := setupFixture(t, messySettings)

	cfg := DefaultConfig()
	cfg.Port = 15722
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}

	// 模拟用户在安装之后又改了别的设置——还原不应把它冲掉。
	after := readSettings(t, dir)
	after, _ = sjson.Set(after, "theme", "dark")
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RestoreSettings(cfg); err != nil {
		t.Fatalf("RestoreSettings: %v", err)
	}
	got := readSettings(t, dir)

	if v := gjson.Get(got, "env.ANTHROPIC_BASE_URL").String(); v != "https://gateway.example.com" {
		t.Errorf("BASE_URL 未还原: %q", v)
	}
	if v := gjson.Get(got, "env.ANTHROPIC_AUTH_TOKEN").String(); v != "sk-ORIGINAL-TOKEN" {
		t.Errorf("AUTH_TOKEN 未还原")
	}
	// 这个键原本不存在，还原后应被删除而非留下空值。
	if gjson.Get(got, "env.CLAUDE_CODE_RETRY_WATCHDOG").Exists() {
		t.Error("CLAUDE_CODE_RETRY_WATCHDOG 原本不存在，还原后应被删除")
	}
	// 用户安装后做的修改必须保留。
	if v := gjson.Get(got, "theme").String(); v != "dark" {
		t.Errorf("还原冲掉了用户后来的修改: theme = %q", v)
	}
}

// 子 Agent 的模型由 CLAUDE.md 中的内置 Agent 规则显式选择。
// ccproxy 不再管理这个全局变量；用户自己设置时，保存和还原都必须原样保留。
func TestSubagentModelIsUserOwned(t *testing.T) {
	dir := setupFixture(t, `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"my-own-model"}}`)
	cfg := DefaultConfig()
	cfg.Port = 15722
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if got := gjson.Get(readSettings(t, dir), "env.CLAUDE_CODE_SUBAGENT_MODEL").String(); got != "my-own-model" {
		t.Fatalf("保存改写了用户的 SUBAGENT_MODEL: %q", got)
	}
	if err := RestoreSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if got := gjson.Get(readSettings(t, dir), "env.CLAUDE_CODE_SUBAGENT_MODEL").String(); got != "my-own-model" {
		t.Errorf("还原改写了用户的 SUBAGENT_MODEL: %q", got)
	}
}

// 最重要的安全阀：读不懂的文件绝不覆盖。
func TestRefusesToTouchBrokenJSON(t *testing.T) {
	dir := setupFixture(t, `{"env": {"A": "b",,, BROKEN`)
	before := readSettings(t, dir)

	cfg := DefaultConfig()
	if _, err := ApplySettings(cfg); err == nil {
		t.Fatal("对损坏的 JSON 应当拒绝写入")
	}
	if got := readSettings(t, dir); got != before {
		t.Error("损坏的文件被修改了")
	}
}

// 文件不存在时应能安全创建。
func TestCreatesWhenAbsent(t *testing.T) {
	dir := setupFixture(t, "")

	cfg := DefaultConfig()
	cfg.Port = 15722
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	got := readSettings(t, dir)
	if v := gjson.Get(got, "env.ANTHROPIC_BASE_URL").String(); v != "http://127.0.0.1:15722" {
		t.Errorf("BASE_URL = %q", v)
	}
}

// 只有 permissions、没有 env 块的配置（Windows 侧的真实形态）。
func TestAddsEnvBlockWithoutDisturbingOthers(t *testing.T) {
	const noEnv = `{
  "permissions": { "defaultMode": "auto" },
  "inputNeededNotifEnabled": true,
  "agentPushNotifEnabled": true
}`
	dir := setupFixture(t, noEnv)

	cfg := DefaultConfig()
	cfg.Port = 15722
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)

	if v := gjson.Get(got, "env.ANTHROPIC_BASE_URL").String(); v != "http://127.0.0.1:15722" {
		t.Errorf("未创建 env 块: %q", v)
	}
	if gjson.Get(got, "permissions.defaultMode").String() != "auto" {
		t.Error("permissions 被破坏")
	}
	if !gjson.Get(got, "inputNeededNotifEnabled").Bool() || !gjson.Get(got, "agentPushNotifEnabled").Bool() {
		t.Error("通知设置被破坏")
	}
}

// firstLines 截取前 n 行，用于让断言失败的输出可读。
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// ---------- 卸载后的纯净状态 ----------

// 备份只该在 ccproxy 第一次接管前产生一份。
// 每次保存都备份的话，用户的 .claude 会按保存次数堆积备份文件，
// 而还原走的是 cfg.Original 逐键回滚，根本不读这些备份。
func TestBackupHappensOnlyOnce(t *testing.T) {
	dir := setupFixture(t, messySettings)
	cfg := DefaultConfig()
	cfg.Providers[0] = Provider{ID: "p1", BaseURL: "https://up.example", Token: "tk"}

	first, err := ApplySettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("首次接管必须留一份原文件备份")
	}
	for i := 0; i < 3; i++ {
		again, err := ApplySettings(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if again != "" {
			t.Errorf("第 %d 次保存又备份了一份: %s", i+2, again)
		}
	}
	if n := countBackups(t, dir); n != 1 {
		t.Errorf("备份文件 = %d 个，四次保存后应当仍只有 1 个", n)
	}
	// 注：上面这条是弱断言——备份名的时间戳只到秒，同一秒内的多次保存
	// 本来就会写到同一个文件名上。真正把关的是循环里那条返回值判定。
}

func countBackups(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if strings.Contains(e.Name(), backupSuffix) {
			n++
		}
	}
	return n
}

// 还原的承诺是「这台机器回到没装过的样子」。
// 只要正在运行的 exe 不在数据目录里，就该一个文件都不剩，连目录一起消失。
func TestPurgeLeavesNothingBehind(t *testing.T) {
	dir := setupFixture(t, messySettings)
	cfg := DefaultConfig()
	cfg.Providers[0] = Provider{ID: "p1", BaseURL: "https://up.example", Token: "sk-REAL-SECRET"}
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	data := mkDataDir(t)
	// 把运行期会出现的每一种文件都摆上，确认没有漏网的
	for _, name := range []string{
		"status.json", "ccproxy.log", "ccproxy.exe", "ccproxy.exe.old",
		tmpPrefix + "1234", "ccproxy.exe.old.old",
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(data, "sub", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}

	purged, leftover, err := PurgeFootprint(cfg.SettingsFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("测试进程不在数据目录里跑，不该有残留: %v", leftover)
	}
	if _, err := os.Stat(purged); !os.IsNotExist(err) {
		rest, _ := os.ReadDir(purged)
		names := make([]string, len(rest))
		for i, r := range rest {
			names[i] = r.Name()
		}
		t.Errorf("数据目录仍在: %s，里面还有 %v", purged, names)
	}
	if n := countBackups(t, dir); n != 0 {
		t.Errorf("settings.json 的备份还剩 %d 个", n)
	}
	// settings.json 本身必须留着——它是用户的文件，不是本程序的。
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Errorf("settings.json 被误删了: %v", err)
	}
}

// 正在运行的 exe 删不掉，必须如实报出来，
// 而不是一边把文件留在那儿一边报告「已清空」。
func TestPurgeReportsRunningExe(t *testing.T) {
	setupFixture(t, messySettings)
	data := mkDataDir(t)
	self := filepath.Join(data, "ccproxy.exe")
	for _, name := range []string{"ccproxy.exe", "config.json", "ccproxy.log", "status.json"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dir, leftover, err := purgeFootprint("", self)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 1 || leftover[0] != self {
		t.Fatalf("leftover = %v，应当恰好是正在运行的那个 exe", leftover)
	}
	// 其余的必须一个不剩——尤其是 config.json，里面是明文凭证。
	rest, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Name() != "ccproxy.exe" {
		names := make([]string, len(rest))
		for i, r := range rest {
			names[i] = r.Name()
		}
		t.Errorf("目录里剩下 %v，除 exe 外都该删掉", names)
	}
}

// 日志不做轮转，超限直接重开——多一档就多一个文件，
// 而这是个承诺「产生的文件都收在 .claude 下」的绿色软件。
func TestLogResetsWhenOversized(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ccproxy.log")
	if err := os.WriteFile(p, make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLogFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 4 {
		t.Errorf("超限后应当从头写，实际 %d 字节", st.Size())
	}

	// 没超限时必须追加，不能把正在排查的日志抹掉
	f2, err := openLogFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.WriteString("more\n"); err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()
	if st, _ := os.Stat(p); st.Size() != 9 {
		t.Errorf("未超限时应当追加，实际 %d 字节", st.Size())
	}
}

// 反复保存不能让用户的文件慢慢长胖。
// json.Indent 会把 src 末尾的空白原样带过去，而我们随后固定补一个换行——
// 不先剪掉的话，每保存一次末尾就多一个空行，几十次之后文件尾巴全是空行。
func TestRepeatedSavesDoNotGrowFile(t *testing.T) {
	dir := setupFixture(t, messySettings)
	cfg := DefaultConfig()
	cfg.Providers[0] = Provider{ID: "p1", BaseURL: "https://u", Token: "t"}

	var prev string
	for i := 0; i < 5; i++ {
		if _, err := ApplySettings(cfg); err != nil {
			t.Fatal(err)
		}
		got := readSettings(t, dir)
		if i > 0 && got != prev {
			t.Fatalf("第 %d 次保存改变了文件（%d -> %d 字节）", i+1, len(prev), len(got))
		}
		prev = got
	}
	if !strings.HasSuffix(prev, "}\n") || strings.HasSuffix(prev, "\n\n") {
		t.Errorf("文件应当以恰好一个换行结尾，实际结尾 %q", prev[len(prev)-4:])
	}
}

// env 原本不存在时才提到首位——sjson 新建的键会追加到文件末尾且不换行，
// 不提上来就是挤在最后的一长行。这是 hoist 存在的唯一理由，
// 也是它唯一该生效的场合。
func TestEnvHoistedOnlyWhenCreated(t *testing.T) {
	dir := setupFixture(t, `{"theme":"dark","permissions":{"allow":[]}}`)
	cfg := DefaultConfig()
	cfg.Providers[0] = Provider{ID: "p1", BaseURL: "https://u", Token: "t"}
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	var order []string
	gjson.Parse(readSettings(t, dir)).ForEach(func(k, _ gjson.Result) bool {
		order = append(order, k.String())
		return true
	})
	if len(order) == 0 || order[0] != "env" {
		t.Fatalf("凭空创建的 env 应当提到首位，实际顺序 %v", order)
	}
	if !strings.Contains(readSettings(t, dir), "\n    \"ANTHROPIC_BASE_URL\"") {
		t.Error("env 块没有按缩进展开，说明还是挤成了一行")
	}
}

// 数据目录里的一切都要删，包括子目录，不给任何东西开后门。
//
// 曾经为 WebView2 的用户数据目录开过一个例外——它当时就建在数据目录下，
// 而卸载是从面板里点的，那一刻它正被打开着删不掉。后来把它挪进了系统临时目录，
// 例外也就没有存在的理由了。这条守的是「别再长回来」。
func TestPurgeRemovesNestedDirs(t *testing.T) {
	setupFixture(t, messySettings)
	data := mkDataDir(t)
	if err := os.MkdirAll(filepath.Join(data, "somedir", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "somedir", "nested", "x.bin"),
		[]byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte(`{"port":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, leftover, err := purgeFootprint("", "")
	if err != nil {
		t.Fatalf("purge 报错: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("应当清空，实际剩下: %v", leftover)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("数据目录本身没删掉: %v", err)
	}
}

// leftover 必须反映「目录里真的还剩什么」，而不是「我打算跳过什么」。
//
// 实测踩过：卸载紧跟 taskkill，被杀的 daemon 还攥着 ccproxy.log 的句柄，
// 删除失败——可 leftover 是按「跳过了哪些」拼的，于是照样报「只剩程序本身」，
// 而目录里其实还躺着日志和一个写了一半的临时文件。
func TestPurgeLeftoverReflectsRealityNotIntent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions cannot simulate a failed delete when running as root")
	}
	setupFixture(t, messySettings)
	data := mkDataDir(t)
	self := filepath.Join(data, "ccproxy.exe")
	if err := os.WriteFile(self, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 一个删得掉的普通文件，和一个删不掉的（用只读父目录模拟被占用）
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(data, "stuck")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "locked"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	// 先建好内容，再收掉父目录的写权限——子项从此删不掉
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	_, leftover, _ := purgeFootprint("", self)

	got := map[string]bool{}
	for _, p := range leftover {
		got[filepath.Base(p)] = true
	}
	if !got["ccproxy.exe"] {
		t.Error("正在运行的 exe 必须出现在 leftover 里")
	}
	if !got["stuck"] {
		t.Error("删不掉的东西必须如实出现在 leftover 里，不能报告成已清空")
	}
	if got["config.json"] {
		t.Error("删掉了的不该还报在 leftover 里")
	}
}

// ---------- 自噬状态：config.json 丢了、settings.json 还指着代理 ----------

// 这是实测踩到的最严重一个：卸载后 settings.json 里 BASE_URL 仍是
// 127.0.0.1:15722、AUTH_TOKEN 仍是 ccproxy-managed，界面却报告「已还原」，
// Claude Code 从此指着一个死地址。
//
// 成因：config.json 没了（换机器、手工清理、数据目录被清空）而 settings.json
// 还被接管着，此时 cfg.Installed 为 false，ApplySettings 就把 ccproxy 自己
// 写的值当成用户原值记进了 Original。
func TestManagedFileNeverBecomesTheOriginal(t *testing.T) {
	// settings.json 已经被上一轮 ccproxy 接管，但 config.json 丢了
	const alreadyManaged = `{
  "theme": "dark",
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:15722",
    "ANTHROPIC_AUTH_TOKEN": "ccproxy-managed",
    "CLAUDE_CODE_RETRY_WATCHDOG": "1",
    "ANTHROPIC_MODEL": "claude-opus-5",
    "MY_OWN_VAR": "keep-me"
  }
}`
	dir := setupFixture(t, alreadyManaged)
	cfg := DefaultConfig() // Installed=false，模拟 config.json 丢失
	cfg.Port = 15722
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSettings(cfg); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)

	for _, k := range []string{
		"env.ANTHROPIC_BASE_URL", "env.ANTHROPIC_AUTH_TOKEN",
		"env.CLAUDE_CODE_RETRY_WATCHDOG", "env.ANTHROPIC_MODEL",
	} {
		if v := gjson.Get(got, k); v.Exists() {
			t.Errorf("%s 仍留着 %q —— 认不出用户原值时必须删键，"+
				"写回一个指向本机代理的死地址是永久损坏", k, v.String())
		}
	}
	// 用户自己的东西一个都不能动
	if gjson.Get(got, "env.MY_OWN_VAR").String() != "keep-me" {
		t.Errorf("误删了用户自己的环境变量: %s", got)
	}
	if gjson.Get(got, "theme").String() != "dark" {
		t.Error("误删了 theme")
	}
}

// 已经被早期版本记坏的 Original，还原时也要能自愈——
// 老用户的 config.json 里已经躺着 {BASE_URL: "http://127.0.0.1:15722"} 了。
func TestRestoreRepairsPoisonedOriginal(t *testing.T) {
	dir := setupFixture(t, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:15722","ANTHROPIC_AUTH_TOKEN":"ccproxy-managed"}}`)
	bad, ph := json.RawMessage(`"http://127.0.0.1:15722"`), json.RawMessage(`"ccproxy-managed"`)
	cfg := DefaultConfig()
	cfg.Installed = true
	cfg.Original = map[string]*json.RawMessage{
		"env.ANTHROPIC_BASE_URL":   &bad, // 早期版本记坏的
		"env.ANTHROPIC_AUTH_TOKEN": &ph,
	}
	if err := RestoreSettings(cfg); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)
	if gjson.Get(got, "env.ANTHROPIC_BASE_URL").Exists() {
		t.Errorf("坏记录被原样写回了: %s", got)
	}
	if gjson.Get(got, "env.ANTHROPIC_AUTH_TOKEN").Exists() {
		t.Errorf("占位符凭证被写回了: %s", got)
	}
}

func TestOriginalPreservesRawJSONTypes(t *testing.T) {
	dir := setupFixture(t, `{"env":{"ANTHROPIC_BASE_URL":123,"ANTHROPIC_AUTH_TOKEN":false,"ANTHROPIC_MODEL":{"custom":true}}}`)
	cfg := DefaultConfig()
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSettings(cfg); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)
	if gjson.Get(got, "env.ANTHROPIC_BASE_URL").Type != gjson.Number ||
		gjson.Get(got, "env.ANTHROPIC_AUTH_TOKEN").Type != gjson.False ||
		!gjson.Get(got, "env.ANTHROPIC_MODEL.custom").Bool() {
		t.Fatalf("original JSON values changed type: %s", got)
	}
}

func TestSetThenClearDeletesWatchdog(t *testing.T) {
	dir := setupFixture(t, `{}`)
	cfg := DefaultConfig()
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.RetryWatchdog = false
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if got := readSettings(t, dir); gjson.Get(got, "env.CLAUDE_CODE_RETRY_WATCHDOG").Exists() {
		t.Fatalf("cleared watchdog remains: %s", got)
	}
}

func TestStrictLocalProxyURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:15722", "HTTP://localhost:1"} {
		if !isLocalProxyURL(raw) {
			t.Errorf("valid URL rejected: %q", raw)
		}
	}
	for _, raw := range []string{"https://127.0.0.1:15722", "http://127.0.0.1:15722/path", "http://localhost:0", "http://localhost:65536", "http://localhost:abc", "http://localhost:15722?x=1", "http://localhost:15722@evil.test"} {
		if isLocalProxyURL(raw) {
			t.Errorf("invalid URL accepted: %q", raw)
		}
	}
}

func TestEmptySlotClearDeletesEnvAndIsNotPersisted(t *testing.T) {
	dir := setupFixture(t, `{}`)
	cfg := DefaultConfig()
	cfg.Slots["main"] = Slot{Provider: "p1", Model: "m"}
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}

	// This is the empty-slot form of the UI clear operation. The API normalizes
	// it before validation and persistence.
	cfg.Slots = normalizeSlots(map[string]Slot{
		"main": {Provider: "p1", Model: ""},
	})
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("clear payload rejected: %v", err)
	}
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if got := readSettings(t, dir); gjson.Get(got, "env.ANTHROPIC_MODEL").Exists() {
		t.Fatalf("cleared ANTHROPIC_MODEL remains: %s", got)
	}
	persisted, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.Slots["main"]; ok {
		t.Fatalf("cleared slot was persisted: %+v", persisted.Slots["main"])
	}
}

// 反过来：文件没被接管时，真实原值必须原样记下并还原。
// 上面两条修的是「认不出就删」，不能顺手把认得出的也删了。
func TestUnmanagedFileKeepsRealOriginal(t *testing.T) {
	dir := setupFixture(t, `{"env":{"ANTHROPIC_BASE_URL":"https://gw.example.com","ANTHROPIC_AUTH_TOKEN":"sk-REAL"}}`)
	cfg := DefaultConfig()
	cfg.Port = 15722
	if _, err := ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSettings(cfg); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)
	if v := gjson.Get(got, "env.ANTHROPIC_BASE_URL").String(); v != "https://gw.example.com" {
		t.Errorf("真实原值没还原: %q", v)
	}
	if v := gjson.Get(got, "env.ANTHROPIC_AUTH_TOKEN").String(); v != "sk-REAL" {
		t.Errorf("真实凭证没还原: %q", v)
	}
}

// mkDataDir 建出数据目录供夹具写文件。
//
// 生产代码里没有「顺手把目录建出来」的函数——那正是当初
// 「卸载后目录自己长回来」的成因，见 dataDirPath 的注释。测试要写夹具，
// 就在测试里显式建。
func mkDataDir(t *testing.T) string {
	t.Helper()
	d, err := dataDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// 还原必须清掉 ccproxy 落在磁盘上的每一样东西。
//
// 这条按真实布局搭夹具：数据目录已经搬到 %LOCALAPPDATA%\ccproxy（测试里由
// sandboxDataDir 指到临时目录），里面除了配置日志还有一个 8 MB 的服务映像；
// settings.json 旁边有安装时留下的备份。面板从别处运行，所以 self 不在
// 数据目录里——没有任何东西可以被「正在运行」这个理由豁免。
func TestPurgeRemovesEverythingInNewLayout(t *testing.T) {
	settingsDir := setupFixture(t, messySettings)
	data := mkDataDir(t)

	// 数据目录里的全套
	for name, content := range map[string]string{
		"config.json":             `{"port":15722,"providers":[{"token":"sk-REAL"}]}`,
		"ccproxy.log":             "listening\n",
		"status.json":             `{"pid":1}`,
		"usage.json":              `{"rows":[]}`,
		"ccproxy-service.exe":     "MZ...",
		"ccproxy-service.exe.old": "MZ old",
		".ccproxy-tmp-abc":        "half written",
	} {
		if err := os.WriteFile(filepath.Join(data, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// settings.json 旁边的安装备份
	backup := filepath.Join(settingsDir, "settings.json"+backupSuffix+"-20260811-141653")
	if err := os.WriteFile(backup, []byte(messySettings), 0o600); err != nil {
		t.Fatal(err)
	}

	// self 指向数据目录之外——正是「启动器放在任意位置」的常态
	dir, leftover, err := purgeFootprint(
		filepath.Join(settingsDir, "settings.json"),
		filepath.Join(t.TempDir(), "Desktop", "ccproxy.exe"))
	if err != nil {
		t.Fatalf("purge 报错: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("应当一个不剩，实际残留: %v", leftover)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("数据目录本身没删掉: %v", err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Error("settings.json 的安装备份没删掉")
	}
	// Claude Code 自己的目录不能被牵连
	if _, err := os.Stat(filepath.Join(settingsDir, "settings.json")); err != nil {
		t.Errorf("settings.json 被删了，还原只该改写它: %v", err)
	}
}
