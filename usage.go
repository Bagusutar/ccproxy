package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// 本文件统计 token 用量并按单价估算花费，数据源是 Claude Code 自己的会话记录
// （<claude 目录>/projects/**/*.jsonl）。
//
// 它不是唯一的用量来源：meter.go 在代理里直接计量经过自己的流量。两者回答
// 的是不同的问题，界面上分开展示，不该合并——
//
//	本文件：Claude Code 一共花了多少，含从没走过 ccproxy 的历史；
//	        但看不到别的客户端（Codex 等）。
//	meter：真正经过 ccproxy 的流量，不挑客户端，还能按上游分；
//	        但只从装上那天算起。
//
// 代理侧计量当初的顾虑是「透传是唯一不能出错的地方」，这条至今成立——
// 所以 meter.go 是在三条硬约束下做的（不攒包、缓冲有上限、解析失败就当没统计到），
// 而不是放弃了那条承诺。细节见该文件。
//
// 会话记录这条路的实测规模：90 个文件 184 MB，全量扫一遍 2.6 秒，
// 之后按 size+mtime 走缓存。

// Price 是每百万 token 的单价。
//
// Cur 为空表示美元。国内厂商按人民币标价，就照人民币存——预先折算成美元
// 会留下 0.14084507042253522 这种残数，输入框装不下、也没人认得出它原本
// 是「1 元」。原价存着，只在算总账时折一次。
type Price struct {
	In     float64 `json:"in"`            // 基础输入
	CacheW float64 `json:"cacheW"`        // 缓存写入
	CacheR float64 `json:"cacheR"`        // 缓存读取（命中）
	Out    float64 `json:"out"`           // 输出
	Cur    string  `json:"cur,omitempty"` // "CNY" 或空（美元）
}

func (p Price) zero() bool { return p.In == 0 && p.CacheW == 0 && p.CacheR == 0 && p.Out == 0 }

// validate checks the persisted custom-price boundary. Zero is a valid price;
// only negative/non-finite values and unsupported currencies are rejected.
func (p Price) validate() error {
	for name, value := range map[string]float64{
		"in": p.In, "cacheW": p.CacheW, "cacheR": p.CacheR, "out": p.Out,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("price %s must be finite and >= 0", name)
		}
	}
	if p.Cur != "" && p.Cur != "CNY" {
		return fmt.Errorf("price currency must be USD (empty) or CNY")
	}
	return nil
}

func validatePrices(prices map[string]Price) error {
	for model, p := range prices {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("price model name must not be empty")
		}
		if strings.TrimSpace(model) != model {
			return fmt.Errorf("price model name %q must not have surrounding whitespace", model)
		}
		if err := p.validate(); err != nil {
			return fmt.Errorf("%q: %w", model, err)
		}
	}
	return nil
}

// usdRate 返回把该币种折成美元要除的系数。
func (p Price) usdRate() float64 {
	if p.Cur == "CNY" {
		return fx
	}
	return 1
}

// cost 按四类 token 算出花费，统一折成美元——一张表里两种货币没法比。
func (p Price) cost(u *ModelUsage) float64 {
	raw := float64(u.In)*p.In + float64(u.CacheW)*p.CacheW +
		float64(u.CacheR)*p.CacheR + float64(u.Out)*p.Out
	return raw / 1e6 / p.usdRate()
}

// ModelUsage 是一个模型的累计用量。
type ModelUsage struct {
	Model  string  `json:"model"`
	Msgs   int     `json:"msgs"`   // 消息数，按 message.id 去重后
	In     int64   `json:"in"`     //
	CacheW int64   `json:"cacheW"` //
	CacheR int64   `json:"cacheR"` //
	Out    int64   `json:"out"`    //
	Cost   float64 `json:"cost"`   // 估算花费，无单价时为 0
	Priced bool    `json:"priced"` // 是否有单价
	Custom bool    `json:"custom"` // 单价是否为用户手填
}

func (u *ModelUsage) total() int64 { return u.In + u.CacheW + u.CacheR + u.Out }

// normalizedUsage is the common internal token shape used by all protocols.
type normalizedUsage struct {
	In     int64
	CacheW int64
	CacheR int64
	Out    int64
}

func normalizeUsage(u gjson.Result) normalizedUsage {
	total := nonnegative(u.Get("input_tokens").Int())
	write := nonnegative(u.Get("cache_creation_input_tokens").Int())
	read := nonnegative(u.Get("cache_read_input_tokens").Int())
	out := nonnegative(u.Get("output_tokens").Int())

	// Responses 把缓存读写放在 input_tokens_details，且 input_tokens 是总输入；
	// Anthropic 的 input_tokens 已经是普通输入，三类输入彼此独立。
	if details := u.Get("input_tokens_details"); details.Exists() {
		write = nonnegative(details.Get("cache_write_tokens").Int())
		read = nonnegative(details.Get("cached_tokens").Int())
		return normalizeTotalUsage(total, write, read, out)
	}
	// Chat Completions 使用 prompt_tokens 及 prompt_tokens_details。
	if prompt := u.Get("prompt_tokens"); prompt.Exists() {
		details := u.Get("prompt_tokens_details")
		return normalizeTotalUsage(prompt.Int(), details.Get("cache_write_tokens").Int(),
			details.Get("cached_tokens").Int(), u.Get("completion_tokens").Int())
	}
	return normalizedUsage{In: total, CacheW: write, CacheR: read, Out: out}
}

func normalizeUsageMap(v map[string]any) normalizedUsage {
	b, _ := json.Marshal(v)
	return normalizeUsage(gjson.ParseBytes(b))
}

func normalizeResponsesUsage(input, write, read, out int64) normalizedUsage {
	return normalizeTotalUsage(input, write, read, out)
}

func normalizeTotalUsage(total, write, read, out int64) normalizedUsage {
	total, write, read, out = nonnegative(total), nonnegative(write), nonnegative(read), nonnegative(out)
	if write > total {
		write = total
	}
	remaining := total - write
	if read > remaining {
		read = remaining
	}
	return normalizedUsage{In: remaining - read, CacheW: write, CacheR: read, Out: out}
}

func nonnegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func anthropicUsage(n normalizedUsage) map[string]any {
	return map[string]any{
		"input_tokens": n.In, "cache_creation_input_tokens": n.CacheW,
		"cache_read_input_tokens": n.CacheR, "output_tokens": n.Out,
	}
}

func chatUsage(n normalizedUsage) map[string]any {
	totalInput := n.In + n.CacheW + n.CacheR
	return map[string]any{
		"prompt_tokens": totalInput, "completion_tokens": n.Out,
		"total_tokens": totalInput + n.Out,
		"prompt_tokens_details": map[string]any{
			"cache_write_tokens": n.CacheW,
			"cached_tokens":      n.CacheR,
		},
	}
}

// fx 是人民币折美元的汇率。取固定值而不去联网查：统计要的是量级，
// 而汇率日间的几个百分点改不了任何判断；联网反而给这个本地工具
// 平添一个会失败、会泄露使用痕迹的外部依赖。
const fx = 6.75

// pricePresets 是内置单价表，按模型名前缀匹配，长的优先。
//
// Claude 与 DeepSeek 取自各自的官方公开价目表（DeepSeek 按 fx 折算）。
// gpt-5.6-* 是中转自建的档位别名，估算采用各档位对应的官方公开价格口径。
// 预设不跟随某个中转的临时倍率；上游实际结算若有差异，可在界面覆盖。
var pricePresets = []struct {
	prefix string
	p      Price
}{
	// usd / cny 两个构造子，币种直接写在调用处——比在五元组末尾补一个
	// "" 或 "CNY" 好认，也不会像位置字面量那样漏了编译器才提醒。
	// —— Anthropic 官方 ——
	{"claude-opus-4-1", usd(15, 18.75, 1.50, 75)},
	{"claude-opus-4-0", usd(15, 18.75, 1.50, 75)},
	{"claude-opus", usd(5, 6.25, 0.50, 25)},
	{"claude-fable", usd(10, 12.50, 1.00, 50)},
	{"claude-mythos", usd(10, 12.50, 1.00, 50)},
	// Sonnet 5 官方在 2026-08-31 前有 $2/$10 的推广价，但中转不一定跟——
	// 实测某中转网关 model_ratio=1.5、completion_ratio=5，收的正是
	// list price $3/$15。预设按 list price 填：低估花费是更坏的那种错
	// （低估让人以为还有余量，高估只是提前警觉），和 estimateTokens
	// 刻意向上取整是同一条取舍。直连官方且在推广期内的，界面上手改即可。
	{"claude-sonnet", usd(3, 3.75, 0.30, 15)},
	{"claude-haiku-3", usd(0.80, 1.00, 0.08, 4)},
	{"claude-haiku", usd(1, 1.25, 0.10, 5)},

	// —— 中转自建 GPT-5.6 档位别名；缓存写 1.25×，缓存读 0.1× ——
	{"gpt-5.6-luna", usd(0.20, 0.25, 0.02, 1.20)},
	{"gpt-5.6-sol", usd(5, 6.25, 0.50, 30)},
	{"gpt-5.6-terra", usd(2, 2.50, 0.20, 12)},

	// —— DeepSeek 官方，人民币原价 ——
	// 它不分缓存写入，未命中的输入就是写入价。
	{"deepseek-v4-flash", cny(1, 1, 0.02, 2)},
	{"deepseek-v4-pro", cny(3, 3, 0.025, 6)},
}

func usd(in, cw, cr, out float64) Price {
	return Price{In: in, CacheW: cw, CacheR: cr, Out: out}
}

func cny(in, cw, cr, out float64) Price {
	return Price{In: in, CacheW: cw, CacheR: cr, Out: out, Cur: "CNY"}
}

// presetFor 查内置单价。找不到返回零值。
//
// 网关常给同一个模型挂多个渠道名，前缀各不相同（实测 acme-claude-opus-5
// 与 claude-opus-5 是同一个模型的两条渠道）。所以先原样查，再剥掉一层
// 自定义前缀重查——否则每加一条渠道就要补一行预设。
func presetFor(model string) Price {
	m := strings.ToLower(strings.TrimSpace(bareModel(model)))
	for _, try := range []string{m, stripVendorPrefix(m)} {
		if try == "" {
			continue
		}
		best := ""
		var found Price
		for _, e := range pricePresets {
			if strings.HasPrefix(try, e.prefix) && len(e.prefix) > len(best) {
				best, found = e.prefix, e.p
			}
		}
		if best != "" {
			return found
		}
	}
	return Price{}
}

// stripVendorPrefix 剥掉 "xxx-" 这一层自定义渠道前缀，
// 只在剥完还剩一个像样的模型名时才算数。
func stripVendorPrefix(m string) string {
	i := strings.Index(m, "-")
	if i <= 0 || i+1 >= len(m) {
		return ""
	}
	return m[i+1:]
}

// ---------- 扫描 ----------

// usageScanner 按文件缓存已统计过的结果。
//
// 文件的 size 和 mtime 都没变就直接用上次的数——会话记录只会追加，
// 历史部分永远不会变。首次全量 2.6 秒，之后只解析新写入的那几个文件。
type usageScanner struct {
	mu    sync.Mutex
	cache map[string]cachedScan
}

type usageKey struct {
	Model string
	ID    string
}

type usageEntry struct {
	Usage normalizedUsage
	At    time.Time
}

type cachedScan struct {
	size      int64
	mtime     time.Time
	prefixSig [32]byte
	suffixSig [32]byte
	entries   map[usageKey]usageEntry
	err       error
}

func newUsageScanner() *usageScanner {
	return &usageScanner{cache: map[string]cachedScan{}}
}

// UsageReport 是给界面的完整结果。
type UsageReport struct {
	Models    []ModelUsage            `json:"models"`
	ByDay     map[string][]ModelUsage `json:"byDay,omitempty"`
	NoDate    []ModelUsage            `json:"noDate,omitempty"`
	Total     ModelUsage              `json:"total"`
	Cost      float64                 `json:"cost"`
	Unpriced  int64                   `json:"unpriced"` // 没有单价的 token 数
	Files     int                     `json:"files"`
	ElapsedMS int64                   `json:"elapsedMs"`
	Root      string                  `json:"root"`
}

// Scan 扫描会话记录目录，按模型汇总用量并计价。
// prices 是用户手填的单价，覆盖内置预设。
func (s *usageScanner) Scan(settingsPath string, prices map[string]Price, ranges ...DateRange) (*UsageReport, error) {
	start := time.Now()
	r := DateRange{}
	if len(ranges) > 0 {
		r = ranges[0]
	}
	if !r.Valid() {
		return nil, fmt.Errorf("invalid date range")
	}
	settingsPath = strings.TrimSpace(settingsPath)
	if settingsPath == "" {
		settingsPath = (&Config{}).SettingsFile()
	}
	root := filepath.Join(filepath.Dir(filepath.Clean(settingsPath)), "projects")
	files := 0
	sources := map[usageKey]usageEntry{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		files++
		c := s.fileUsage(p, st)
		if c.err != nil {
			return fmt.Errorf("scan %s: %w", p, c.err)
		}
		mergeEntries(sources, c.entries)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	resolveModelLessEntries(sources)
	agg := map[string]*ModelUsage{}
	byDay := map[string]map[string]*ModelUsage{}
	noDate := map[string]*ModelUsage{}
	rep := &UsageReport{Files: files, Root: root, ByDay: map[string][]ModelUsage{}}
	for key, e := range sources {
		if e.Usage.In+e.Usage.CacheW+e.Usage.CacheR+e.Usage.Out == 0 {
			continue
		}
		u := &ModelUsage{Model: key.Model, Msgs: 1, In: e.Usage.In, CacheW: e.Usage.CacheW, CacheR: e.Usage.CacheR, Out: e.Usage.Out}
		if e.At.IsZero() {
			mergeOne(noDate, key.Model, u)
		} else {
			day := localDay(e.At)
			if byDay[day] == nil {
				byDay[day] = map[string]*ModelUsage{}
			}
			mergeOne(byDay[day], key.Model, u)
		}
	}
	for day, models := range byDay {
		for _, u := range models {
			if r.Includes(day) {
				if rep.ByDay[day] == nil {
					rep.ByDay[day] = []ModelUsage{}
				}
				rep.ByDay[day] = append(rep.ByDay[day], *u)
			}
		}
	}
	for _, u := range noDate {
		rep.NoDate = append(rep.NoDate, *u)
	}
	// No-date entries are included only in the all-time aggregate.
	for day, models := range byDay {
		if r.Includes(day) {
			for m, u := range models {
				mergeOne(agg, m, u)
			}
		}
	}
	if r.From == "" && r.To == "" {
		for m, u := range noDate {
			mergeOne(agg, m, u)
		}
	}
	for _, u := range agg {
		if u.total() == 0 {
			continue
		}
		p, custom := prices[u.Model]
		if !custom {
			p = presetFor(u.Model)
		}
		if custom || !p.zero() {
			u.Cost = p.cost(u)
			u.Priced = true
			u.Custom = custom
		} else {
			rep.Unpriced += u.total()
		}
		rep.Cost += u.Cost
		rep.Total.Msgs += u.Msgs
		rep.Total.In += u.In
		rep.Total.CacheW += u.CacheW
		rep.Total.CacheR += u.CacheR
		rep.Total.Out += u.Out
		rep.Models = append(rep.Models, *u)
	}
	rep.Total.Cost = rep.Cost
	for day, rows := range rep.ByDay {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
		rep.ByDay[day] = rows
	}
	sort.Slice(rep.Models, func(i, j int) bool {
		return rep.Models[i].Cost > rep.Models[j].Cost || (rep.Models[i].Cost == rep.Models[j].Cost && rep.Models[i].total() > rep.Models[j].total())
	})
	sort.Slice(rep.NoDate, func(i, j int) bool { return rep.NoDate[i].Model < rep.NoDate[j].Model })
	rep.ElapsedMS = time.Since(start).Milliseconds()
	return rep, nil
}
func mergeOne(dst map[string]*ModelUsage, m string, u *ModelUsage) {
	t := dst[m]
	if t == nil {
		t = &ModelUsage{Model: m}
		dst[m] = t
	}
	t.Msgs += u.Msgs
	t.In += u.In
	t.CacheW += u.CacheW
	t.CacheR += u.CacheR
	t.Out += u.Out
}
func mergeUsageEntry(dst map[usageKey]usageEntry, key usageKey, incoming usageEntry) {
	current, ok := dst[key]
	if !ok {
		dst[key] = incoming
		return
	}
	currentInput := current.Usage.In + current.Usage.CacheW + current.Usage.CacheR
	incomingInput := incoming.Usage.In + incoming.Usage.CacheW + incoming.Usage.CacheR
	if incomingInput > currentInput {
		current.Usage.In = incoming.Usage.In
		current.Usage.CacheW = incoming.Usage.CacheW
		current.Usage.CacheR = incoming.Usage.CacheR
	}
	maxInto(&current.Usage.Out, incoming.Usage.Out)
	if !incoming.At.IsZero() && (current.At.IsZero() || incoming.At.Before(current.At)) {
		current.At = incoming.At
	}
	dst[key] = current
}

func mergeEntries(dst, src map[usageKey]usageEntry) {
	for key, incoming := range src {
		mergeUsageEntry(dst, key, incoming)
	}
}

func resolveModelLessEntries(entries map[usageKey]usageEntry) {
	modelsByID := map[string]map[string]bool{}
	for key := range entries {
		if key.Model == "" {
			continue
		}
		if modelsByID[key.ID] == nil {
			modelsByID[key.ID] = map[string]bool{}
		}
		modelsByID[key.ID][key.Model] = true
	}
	for key, entry := range entries {
		if key.Model != "" {
			continue
		}
		models := modelsByID[key.ID]
		if len(models) == 1 {
			for model := range models {
				mergeUsageEntry(entries, usageKey{Model: model, ID: key.ID}, entry)
			}
		} else {
			// Preserve billable usage without guessing when an ID has no model hint
			// or conflicting hints across files. The row remains visibly unpriced.
			mergeUsageEntry(entries, usageKey{Model: "(未指定)", ID: key.ID}, entry)
		}
		delete(entries, key)
	}
}

func usageFileSignature(path string, size int64) (prefix, suffix [32]byte) {
	f, err := os.Open(path)
	if err != nil {
		return prefix, suffix
	}
	defer f.Close()
	const sample = 4 << 10
	first := make([]byte, min(int64(sample), size))
	n, _ := io.ReadFull(f, first)
	prefix = sha256.Sum256(first[:n])
	if size > sample {
		last := make([]byte, min(int64(sample), size))
		_, _ = f.ReadAt(last, max(int64(0), size-int64(len(last))))
		suffix = sha256.Sum256(last)
	} else {
		suffix = prefix
	}
	return prefix, suffix
}

func (s *usageScanner) fileUsage(path string, st os.FileInfo) cachedScan {
	prefix, suffix := usageFileSignature(path, st.Size())
	s.mu.Lock()
	c, ok := s.cache[path]
	s.mu.Unlock()
	if ok && c.size == st.Size() && c.mtime.Equal(st.ModTime()) && c.prefixSig == prefix && c.suffixSig == suffix {
		return c
	}
	entries, err := parseUsageEntries(path)
	c = cachedScan{size: st.Size(), mtime: st.ModTime(), prefixSig: prefix, suffixSig: suffix, entries: entries, err: err}
	s.mu.Lock()
	s.cache[path] = c
	s.mu.Unlock()
	return c
}

func parseUsageEntries(path string) (map[usageKey]usageEntry, error) {
	entries := map[usageKey]usageEntry{}
	f, err := os.Open(path)
	if err != nil {
		return entries, err
	}
	defer f.Close()

	const maxLine = 64 << 20
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		var line []byte
		overlong := false
		var readErr error
		for {
			var chunk []byte
			chunk, readErr = r.ReadSlice('\n')
			if !overlong {
				if len(line)+len(chunk) > maxLine {
					overlong = true
					line = nil
				} else {
					line = append(line, chunk...)
				}
			}
			if readErr != bufio.ErrBufferFull {
				break
			}
		}
		if readErr != nil && readErr != io.EOF {
			return entries, readErr
		}
		if overlong {
			if readErr == io.EOF {
				return entries, fmt.Errorf("JSONL line exceeds %d bytes", maxLine)
			}
			continue
		}
		b := bytes.TrimSuffix(line, []byte{'\n'})
		if len(b) > 0 && bytes.Contains(b, []byte(`"message"`)) {
			msg := gjson.GetBytes(b, "message")
			id := msg.Get("id").String()
			if id != "" {
				model := msg.Get("model").String()
				u := msg.Get("usage")
				if model != "" || u.Exists() {
					incoming := usageEntry{}
					if u.Exists() {
						incoming.Usage = normalizeUsage(u)
					}
					if ts := gjson.GetBytes(b, "timestamp").String(); ts != "" {
						incoming.At, _ = time.Parse(time.RFC3339Nano, ts)
					}
					mergeUsageEntry(entries, usageKey{Model: model, ID: id}, incoming)
				}
			}
		}
		if readErr == io.EOF {
			return entries, nil
		}
	}
}

// parseUsageFile keeps the file-level test seam while using the same
// (model,message.id) entries as the global scanner.
func parseUsageFile(path string) (map[string]map[string]*ModelUsage, map[string]*ModelUsage, error) {
	byDay := map[string]map[string]*ModelUsage{}
	noDate := map[string]*ModelUsage{}
	entries, err := parseUsageEntries(path)
	if err != nil {
		return nil, nil, err
	}
	for key, entry := range entries {
		u := &ModelUsage{Model: key.Model, Msgs: 1, In: entry.Usage.In, CacheW: entry.Usage.CacheW, CacheR: entry.Usage.CacheR, Out: entry.Usage.Out}
		if entry.At.IsZero() {
			mergeOne(noDate, key.Model, u)
			continue
		}
		day := localDay(entry.At)
		if byDay[day] == nil {
			byDay[day] = map[string]*ModelUsage{}
		}
		mergeOne(byDay[day], key.Model, u)
	}
	return byDay, noDate, nil
}

func maxInto(dst *int64, v int64) {
	if v > *dst {
		*dst = v
	}
}
