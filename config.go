package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Upstream 描述一个上游 API 端点。仅用于从旧版配置迁移。
type Upstream struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"`
}

// Provider 是一个上游。数量不限，用户自行增删。
//
// 两个地址对应两套协议。BaseURL 是必填的 Anthropic 端点——Claude Code
// 只会说这一种协议。OpenAIBaseURL 选填，留空即与 BaseURL 相同；
// 只有当上游把两种协议挂在不同路径下时才需要分开填，
// 例如 DeepSeek 的 Anthropic 端点在 /anthropic 下，OpenAI 端点在根路径。
type Provider struct {
	ID            string   `json:"id"`   // 稳定标识，UI 增删与槽位引用都靠它
	Name          string   `json:"name"` // 显示名
	BaseURL       string   `json:"baseUrl"`
	OpenAIBaseURL string   `json:"openaiBaseUrl,omitempty"`
	Token         string   `json:"token"`
	Models        []string `json:"models"`      // 上次探测 /v1/models 的结果，供下拉选择
	CountTokens   bool     `json:"countTokens"` // 探测到的能力：是否支持 /v1/messages/count_tokens

	// ModelProtocols 记录每个模型原生可用的 API 方言，由连通性测试写入。
	//
	// 固化在配置里而不是每次请求现探：探测要发真实请求，放在请求路径上
	// 既慢又花钱，还会让同一个模型在不同时刻得到不同结论。写进文件后
	// 下次启动直接用，除非用户重新测试。
	//
	// 键是模型名，值是方言名（anthropic / chat / responses）。
	// 模型不在表里 = 尚未测过，不等于不支持。
	ModelProtocols map[string][]string `json:"modelProtocols,omitempty"`

	Default bool `json:"default,omitempty"` // 未知模型的兜底上游
}

// ProtocolsFor 返回某模型已记录的原生方言。未测过时返回 nil。
func (p *Provider) ProtocolsFor(model string) []Protocol {
	raw, ok := p.ModelProtocols[bareModel(model)]
	if !ok {
		return nil
	}
	out := make([]Protocol, 0, len(raw))
	for _, s := range raw {
		out = append(out, Protocol(s))
	}
	return out
}

// SupportsProtocol 判断某模型是否原生支持某方言。
func (p *Provider) SupportsProtocol(model string, proto Protocol) bool {
	for _, x := range p.ProtocolsFor(model) {
		if x == proto {
			return true
		}
	}
	return false
}

// BaseForProtocol 按目标方言选地址。
//
// 注意判据是「要用哪种方言发出去」，不是「客户端用哪个路径进来的」——
// 翻译引入之后这两者会分叉：客户端发 /v1/messages，目标可能是上游的
// /v1/responses，此时该用 OpenAI 地址。
func (p *Provider) BaseForProtocol(proto Protocol) string {
	alt := strings.TrimSpace(p.OpenAIBaseURL)
	if alt != "" && proto.isOpenAI() {
		return alt
	}
	return p.BaseURL
}

// Slot 是一个 Claude Code 模型位的赋值。
type Slot struct {
	Provider string `json:"provider"` // Provider.ID
	Model    string `json:"model"`    // 裸模型名，不含 [1M]
	OneM     bool   `json:"oneM"`     // 是否附加 [1M]（Claude Code 会转成 1M context beta 头）
}

// slotEnvKeys 把槽位映射到 Claude Code 的环境变量名。
// 顺序即界面展示顺序；Group 用于分组，说明取自官方文档。
var slotEnvKeys = []struct{ Key, Env, Label, Hint, Group string }{
	{"main", "ANTHROPIC_MODEL", "主模型",
		"会话启动时使用的模型，等同 /model 选中的那个", "direct"},
	{"opus", "ANTHROPIC_DEFAULT_OPUS_MODEL", "opus",
		"Plan Mode 下的 opusplan 也解析到这里", "alias"},
	{"sonnet", "ANTHROPIC_DEFAULT_SONNET_MODEL", "sonnet",
		"非 Plan Mode 的 opusplan 解析到这里", "alias"},
	{"haiku", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "haiku",
		"同时用于后台功能：为 --resume 生成会话摘要等", "alias"},
	{"fable", "ANTHROPIC_DEFAULT_FABLE_MODEL", "fable",
		"也是第三方上游自动降级时识别 Fable 5 的依据", "alias"},
}

// Config 是 ccproxy 的唯一真相源，存放于 <claude 配置目录>/ccproxy/config.json。
// GUI 进程写入，daemon 进程监听 mtime 热重载。
//
// 路由规则：请求体的 model 字段去掉 [1M] 后缀后，
// 先查槽位赋值，再查各上游探测到的模型列表，都不中则走默认上游。
type Config struct {
	Port int `json:"port"`

	Providers []Provider      `json:"providers"`
	Slots     map[string]Slot `json:"slots"`

	// 仅用于从旧版配置迁移，新配置不再写入。
	Gateway  *Upstream `json:"gateway,omitempty"`
	DeepSeek *Upstream `json:"deepseek,omitempty"`

	RetryWatchdog bool `json:"retryWatchdog"` // 是否写入 CLAUDE_CODE_RETRY_WATCHDOG=1

	// FirstByteSec 上游多久未返回响应头就重发。必须明显小于 Claude Code
	// 自身的 300 秒阈值，也要小于常见网关的 120 秒 Proxy Read Timeout。
	FirstByteSec int `json:"firstByteSec"`
	// StallSec 上游开流后多久无任何数据（含 SSE ping）就提前结束该流。
	StallSec int `json:"stallSec"`

	// Prices 是用户手填的单价，键是模型名，覆盖 usage.go 里的内置预设。
	// 只存被改过的那些——没改过的跟着预设走，预设更新了自动生效。
	Prices map[string]Price `json:"prices,omitempty"`

	// SettingsPath 指定要改写的 settings.json。留空则用系统默认路径。
	// 用于两类情况：目标是 WSL 里的配置（\\wsl.localhost\...），
	// 或用户设过 CLAUDE_CONFIG_DIR 把配置放在了别处。
	SettingsPath string `json:"settingsPath,omitempty"`

	Installed             bool   `json:"installed"`
	InstalledSettingsPath string `json:"installedSettingsPath,omitempty"`
	BackupPath            string `json:"backupPath"`
	InstalledAt           string `json:"installedAt"`

	// Original 记录安装前每个受管键的原始 JSON 值。
	// nil 表示该键原本不存在，还原时应删除而非置空。
	Original map[string]*json.RawMessage `json:"original,omitempty"`
}

const (
	defaultPort       = 15722
	defaultFirstByte  = 95
	defaultStall      = 60
	maxTimeoutSeconds = 24 * 60 * 60
)

var providerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validPort(port int) bool { return port >= 1 && port <= 65535 }

func validTimeoutSeconds(seconds int) bool {
	return seconds > 0 && seconds <= maxTimeoutSeconds
}

// normalizeSlots turns an empty optional model into the canonical persisted
// representation: no map entry. Unknown slots remain for validation to reject.
func normalizeSlots(slots map[string]Slot) map[string]Slot {
	if slots == nil {
		return map[string]Slot{}
	}
	for _, sd := range slotEnvKeys {
		if slot, ok := slots[sd.Key]; ok && strings.TrimSpace(slot.Model) == "" {
			delete(slots, sd.Key)
		}
	}
	return slots
}

func validateConfig(c *Config) error {
	if !validPort(c.Port) {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if !validTimeoutSeconds(c.FirstByteSec) {
		return fmt.Errorf("firstByteSec must be between 1 and %d", maxTimeoutSeconds)
	}
	if !validTimeoutSeconds(c.StallSec) {
		return fmt.Errorf("stallSec must be between 1 and %d", maxTimeoutSeconds)
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider is required")
	}
	ids := make(map[string]struct{}, len(c.Providers))
	for i := range c.Providers {
		id := c.Providers[i].ID
		if !providerIDPattern.MatchString(id) {
			return fmt.Errorf("provider %d has invalid id %q", i+1, id)
		}
		if _, exists := ids[id]; exists {
			return fmt.Errorf("duplicate provider id %q", id)
		}
		ids[id] = struct{}{}
	}
	knownSlots := make(map[string]struct{}, len(slotEnvKeys))
	for _, sd := range slotEnvKeys {
		knownSlots[sd.Key] = struct{}{}
	}
	for key, slot := range c.Slots {
		// subagent is a routing-only compatibility slot from older ccproxy
		// configurations. Claude Code's global SUBAGENT_MODEL remains user-owned;
		// accepting this key must not make ApplySettings rewrite that env value.
		if key == "subagent" {
			if strings.TrimSpace(slot.Model) == "" {
				return fmt.Errorf("slot %q has no model", key)
			}
			if _, ok := ids[slot.Provider]; !ok {
				return fmt.Errorf("slot %q references unknown provider %q", key, slot.Provider)
			}
			continue
		}
		if _, ok := knownSlots[key]; !ok {
			return fmt.Errorf("unknown slot %q", key)
		}
		if strings.TrimSpace(slot.Model) == "" {
			return fmt.Errorf("slot %q has an empty model", key)
		}
		if _, ok := ids[slot.Provider]; !ok {
			return fmt.Errorf("slot %q references unknown provider %q", key, slot.Provider)
		}
	}
	return validatePrices(c.Prices)
}

// DefaultConfig 返回一份空白但可用的配置：一个待填的默认上游。
func DefaultConfig() *Config {
	return &Config{
		Port:          defaultPort,
		Providers:     []Provider{{ID: "p1"}},
		Slots:         map[string]Slot{},
		RetryWatchdog: true,
		FirstByteSec:  defaultFirstByte,
		StallSec:      defaultStall,
	}
}

// DefaultProvider 返回兜底上游：列表中的第一个。
func (c *Config) DefaultProvider() *Provider {
	if len(c.Providers) > 0 {
		return &c.Providers[0]
	}
	return nil
}

func (c *Config) ProviderByID(id string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}

// migrate 兼容旧版配置结构。
func (c *Config) migrate() {
	if len(c.Providers) == 0 {
		if c.Gateway != nil && c.Gateway.BaseURL != "" {
			c.Providers = append(c.Providers, Provider{
				ID:      "p1",
				BaseURL: c.Gateway.BaseURL, Token: c.Gateway.Token,
			})
		}
		if c.DeepSeek != nil && c.DeepSeek.BaseURL != "" {
			c.Providers = append(c.Providers, Provider{
				ID:      "p2",
				BaseURL: c.DeepSeek.BaseURL, Token: c.DeepSeek.Token,
			})
		}
	}
	if len(c.Providers) == 0 {
		c.Providers = DefaultConfig().Providers
	}
	// Repair legacy missing/invalid/duplicate IDs while preserving every valid ID.
	used := map[string]bool{}
	reserved := map[string]bool{}
	for _, p := range c.Providers {
		if providerIDPattern.MatchString(p.ID) {
			reserved[p.ID] = true
		}
	}
	remap := map[string]string{}
	next := 1
	for i := range c.Providers {
		old, id := c.Providers[i].ID, c.Providers[i].ID
		if !providerIDPattern.MatchString(id) || used[id] {
			for {
				id = fmt.Sprintf("p%d", next)
				next++
				if !used[id] && !reserved[id] {
					break
				}
			}
			c.Providers[i].ID = id
		}
		used[id] = true
		if _, exists := remap[old]; !exists {
			remap[old] = id
		}
	}
	// 兜底上游不由界面控制：列表中的第一个即兜底。
	// 少一个旋钮，语义也更直观——顺序就是优先级。
	for i := range c.Providers {
		c.Providers[i].Default = i == 0
	}
	c.Slots = normalizeSlots(c.Slots)
	for key, slot := range c.Slots {
		if id, ok := remap[slot.Provider]; ok {
			slot.Provider = id
			c.Slots[key] = slot
		}
	}
	if c.FirstByteSec == 0 {
		c.FirstByteSec = defaultFirstByte
	}
	if c.StallSec == 0 {
		c.StallSec = defaultStall
	}
	c.Gateway, c.DeepSeek = nil, nil
}

// claudeDir 返回 Claude Code 的配置目录，规则与 Claude Code 自身一致：
// CLAUDE_CONFIG_DIR 优先，否则 ~/.claude（Windows 上即 %USERPROFILE%\.claude）。
//
// ccproxy 对这个目录只读不写，唯一的例外是它本来就该改写的 settings.json。
// 用途有两处：定位 settings.json，以及扫描 projects/ 下的会话记录做用量统计。
// 自己的配置、日志、服务映像一律不放这里——见 dataDirPath。
func claudeDir() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// dataDirPath 返回 ccproxy 自己的数据目录。不创建它。
//
//	Windows   %LOCALAPPDATA%\ccproxy
//	其他       $XDG_CONFIG_HOME/ccproxy（未设时 ~/.config/ccproxy）
//
// 曾经放在 <claude dir>/ccproxy 下，理由是「运行产生的一切都收在 Claude Code
// 的配置目录里，不散落到别处」。这条理由站不住：那个目录属于 Claude Code，
// 而用户完全可能让 ccproxy 去管另一个位置的 settings.json——此时数据仍落在
// 默认的 .claude 下，就成了「我明明指向别处，它却在这儿建了东西」。
//
// %LOCALAPPDATA% 才是 Windows 上放每用户本机数据的正确位置：不随域账户漫游
// （里面有真实凭证，也有一个 8 MB 的可执行文件，两者都不该被同步到别的机器），
// 且不需要任何权限。用户手上那个启动器 exe 仍然放在任意位置、不被任何进程占用；
// 被占用的只有这里的服务映像。
//
// 不创建是刻意的。曾经有一个会创建的变体，于是「卸载并还原」刚把目录删干净，
// 界面 5 秒一次的状态轮询就顺手把空目录建了回来——用户看到的是
// 「说是删了，回头一看还在」。建目录的资格只属于真要写文件的那一处，
// 现在只有 atomicWrite 和 InstallService 有。
func dataDirPath() (string, error) {
	if runtime.GOOS == "windows" {
		// 直接读环境变量而不用 os.UserCacheDir：后者在 Windows 上返回的
		// 确实是 %LocalAppData%，但名字写着 Cache，读代码的人会以为
		// 这些文件是可丢弃的缓存——而这里放着凭证和服务映像。
		if p := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); p != "" {
			return filepath.Join(p, "ccproxy"), nil
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ccproxy"), nil
}

func configPath() (string, error) {
	dir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// LoadConfig 读取配置；文件不存在时返回默认配置而非报错。
func LoadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	cfg.migrate()
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// SaveConfig 原子写入配置，权限 0600（内含 API key）。
func SaveConfig(cfg *Config) error {
	if err := validateConfig(cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	p, err := configPath()
	if err != nil {
		return err
	}
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(p, buf, 0o600)
}

// tmpPrefix 是原子写入的临时文件前缀。
//
// 用带产品名的前缀而不是通用的 .tmp-：这些文件写在目标文件的同级目录，
// 而 atomicWrite 的目标不止一处（%LOCALAPPDATA%\ccproxy 下的配置与用量，
// 以及用户指定的 settings.json 旁边）。进程若在写入与 rename 之间被杀，
// 残片会留下来，而「卸载后回到干净状态」要求我们认得出哪些是自己的。
const tmpPrefix = ".ccproxy-tmp-"

// atomicWrite 先写同目录临时文件再 rename，避免写入过程中断导致配置损坏。
//
// 顺带负责建目录：读路径现在一律不建（否则卸载后会被轮询重建回来），
// 所以「确保目录存在」这件事就落在真正要写文件的这一层。
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	// Windows 上 os.Rename 可覆盖已存在文件（底层为 MoveFileEx + REPLACE_EXISTING）。
	return os.Rename(tmpName, path)
}

// Status 是 daemon 周期性写出的运行状态，供 GUI 读取。
type Status struct {
	PID       int               `json:"pid"`
	Port      int               `json:"port"`
	Nonce     string            `json:"nonce"`
	StartedAt time.Time         `json:"startedAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Hits      map[string]uint64 `json:"hits"`
	LastError string            `json:"lastError,omitempty"`
}

func statusPath() (string, error) {
	dir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "status.json"), nil
}

var statusMu sync.Mutex

// ClearStatus 删除状态文件。
//
// 停止代理后必须调用：status.json 里的 UpdatedAt 在进程被杀的那一刻
// 仍然是新鲜的，ReadStatus 会继续判定「运行中」长达 30 秒。
// 删掉文件才是即时且明确的「已停止」信号。
// daemon 若其实没死，下一次心跳会把文件写回来——这正是我们要的自愈行为。
func ClearStatus() error {
	statusMu.Lock()
	defer statusMu.Unlock()
	p, err := statusPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func WriteStatus(s *Status) error {
	statusMu.Lock()
	defer statusMu.Unlock()
	p, err := statusPath()
	if err != nil {
		return err
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(p, buf, 0o600)
}

// ReadStatus 读取 daemon 状态；不存在或过期均视为未运行。
func ReadStatus() (*Status, bool) {
	p, err := statusPath()
	if err != nil {
		return nil, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false
	}
	// 状态超过 30 秒未刷新视为进程已退出（daemon 每 5 秒刷新一次）。
	if time.Since(s.UpdatedAt) > 30*time.Second {
		return &s, false
	}
	return &s, true
}

func logPath() (string, error) {
	dir, err := dataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ccproxy.log"), nil
}
