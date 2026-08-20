package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Claude Code 每个内容块写一条记录，每条都带整条消息的同一份 usage。
// 逐条累加就是把一条消息算 N 遍——实测让主线用量虚高 2.1 倍，
// 而且虚高倍数正比于工具调用密度，恰好系统性地夸大子 Agent 那一侧。
func TestUsageDedupesByMessageID(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "demo")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	// 一条消息写了 3 条记录（thinking / text / tool_use），usage 完全相同
	const one = `{"type":"assistant","message":{"id":"m1","model":"claude-opus-5",` +
		`"usage":{"input_tokens":10,"cache_creation_input_tokens":100,` +
		`"cache_read_input_tokens":1000,"output_tokens":50}}}`
	// 另一条消息，另一个模型
	const two = `{"type":"assistant","message":{"id":"m2","model":"deepseek-v4-flash",` +
		`"usage":{"input_tokens":7,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":70,"output_tokens":3}}}`
	body := one + "\n" + one + "\n" + one + "\n" + two + "\n" +
		`{"type":"user","message":{"content":"无 usage 的行应当被跳过"}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sandboxDataDir(t)

	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]ModelUsage{}
	for _, m := range rep.Models {
		byModel[m.Model] = m
	}
	o := byModel["claude-opus-5"]
	if o.Msgs != 1 {
		t.Errorf("消息数 = %d，三条记录同一个 message.id，应当算 1 条", o.Msgs)
	}
	if o.In != 10 || o.CacheW != 100 || o.CacheR != 1000 || o.Out != 50 {
		t.Errorf("token 被重复累加了: %+v", o)
	}
	// 预设单价应当自动命中：opus-5 = 10×$5 + 100×$6.25 + 1000×$0.50 + 50×$25 每百万
	want := (10*5.0 + 100*6.25 + 1000*0.50 + 50*25.0) / 1e6
	if !o.Priced || o.Cost < want*0.999 || o.Cost > want*1.001 {
		t.Errorf("花费 = %v，期望 %v (priced=%v)", o.Cost, want, o.Priced)
	}
	if d := byModel["deepseek-v4-flash"]; !d.Priced || d.Cost <= 0 {
		t.Errorf("DeepSeek 预设未命中: %+v", d)
	}
	if rep.Unpriced != 0 {
		t.Errorf("两个模型都有预设，不该有未计价 token: %d", rep.Unpriced)
	}
}

// 手填单价必须覆盖预设；没有预设的模型如实标记为未计价。
func TestUsageCustomPriceOverridesPreset(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "demo")
	os.MkdirAll(proj, 0o700)
	body := `{"type":"assistant","message":{"id":"a","model":"claude-opus-5",` +
		`"usage":{"input_tokens":1000000,"output_tokens":0,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n" +
		`{"type":"assistant","message":{"id":"b","model":"某个没人认识的模型",` +
		`"usage":{"input_tokens":500,"output_tokens":0,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(body), 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sandboxDataDir(t)

	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), map[string]Price{
		"claude-opus-5": {In: 1, CacheW: 1, CacheR: 1, Out: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rep.Models {
		switch m.Model {
		case "claude-opus-5":
			if !m.Custom || m.Cost < 0.999 || m.Cost > 1.001 {
				t.Errorf("手填单价没覆盖预设（预设是 $5，手填 $1，百万输入应当是 $1）: %+v", m)
			}
		default:
			if m.Priced {
				t.Errorf("不认识的模型不该凭空猜单价: %+v", m)
			}
			if rep.Unpriced != 500 {
				t.Errorf("未计价 token = %d，期望 500", rep.Unpriced)
			}
		}
	}
}

// 网关常给同一个模型挂多条渠道、各带不同前缀。
// 预设要能剥掉一层前缀命中，否则每加一条渠道都得补一行。
func TestPresetMatchesVendorPrefixedNames(t *testing.T) {
	for _, c := range []struct {
		model           string
		in, cw, cr, out float64
	}{
		{"claude-opus-5", 5, 6.25, 0.50, 25},
		{"acme-claude-opus-5", 5, 6.25, 0.50, 25},
		{"gpt-5.6-luna", 0.20, 0.25, 0.02, 1.20},
		{"acme-gpt-5.6-luna", 0.20, 0.25, 0.02, 1.20},
		{"gpt-5.6-sol", 5, 6.25, 0.50, 30},
		{"gpt-5.6-terra", 2, 2.50, 0.20, 12},
		{"claude-opus-5[1m]", 5, 6.25, 0.50, 25}, // 档位后缀不影响匹配
		{"claude-haiku-4-5", 1, 1.25, 0.10, 5},
		{"claude-opus-4-1", 15, 18.75, 1.50, 75}, // 退役机型是旧价，别被前缀盖住
		{"claude-opus-4-8", 5, 6.25, 0.50, 25},   // 未单列的新机型落到 claude-opus
		{"claude-sonnet-5", 3, 3.75, 0.30, 15},
	} {
		if got := presetFor(c.model); got.In != c.in || got.CacheW != c.cw ||
			got.CacheR != c.cr || got.Out != c.out {
			t.Errorf("%s 的单价 = %+v，期望 in=%v cw=%v cr=%v out=%v",
				c.model, got, c.in, c.cw, c.cr, c.out)
		}
	}
	if !presetFor("完全不认识").zero() {
		t.Error("不认识的模型必须返回零值，不能瞎猜")
	}
}

// 人民币标价的模型按原价存，只在算总账时折一次。
// 折算漏了就是 6.75 倍的差错，而 DeepSeek 恰好是最容易被忽略的那一侧
// （它占花费的比例极小，数字错了也不显眼）。
func TestCNYPricesConvertToUSD(t *testing.T) {
	u := &ModelUsage{In: 1_000_000}

	// 预设里 deepseek-v4-flash 的未命中输入是 ¥1/百万
	p := presetFor("deepseek-v4-flash")
	if p.Cur != "CNY" {
		t.Fatalf("币种 = %q，DeepSeek 应当按人民币存原价", p.Cur)
	}
	if p.In != 1 {
		t.Errorf("单价 = %v，应当是人民币原价 1 而不是折算后的残数", p.In)
	}
	want := 1.0 / 6.75
	if got := p.cost(u); got < want*0.999 || got > want*1.001 {
		t.Errorf("百万输入 = $%v，期望 $%v（¥1 ÷ 6.75）", got, want)
	}

	// 美元标价的不折
	o := presetFor("claude-opus-5")
	if o.Cur != "" {
		t.Errorf("Claude 系应当是美元（空币种），实际 %q", o.Cur)
	}
	if got := o.cost(u); got < 4.999 || got > 5.001 {
		t.Errorf("百万输入 = $%v，期望 $5", got)
	}
}

// 手填价必须连币种一起生效：只认数字的话，一个按人民币填的价会被当成
// 美元算，差 6.75 倍——而且界面上看不出任何异样。
func TestCustomPriceRespectsCurrency(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "d")
	os.MkdirAll(proj, 0o700)
	os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(
		`{"type":"assistant","message":{"id":"a","model":"某国产模型",`+
			`"usage":{"input_tokens":1000000,"cache_creation_input_tokens":0,`+
			`"cache_read_input_tokens":0,"output_tokens":0}}}`+"\n"), 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sandboxDataDir(t)

	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), map[string]Price{
		"某国产模型": {In: 6.75, Cur: "CNY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Models) != 1 {
		t.Fatalf("模型数 = %d", len(rep.Models))
	}
	if c := rep.Models[0].Cost; c < 0.999 || c > 1.001 {
		t.Errorf("花费 = $%v，期望 $1（¥6.75 ÷ 6.75）", c)
	}
}

func TestUsageScannerUsesExplicitSettingsRootsWithoutCrossContamination(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(filepath.Join(root, "projects", "p"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, id, model string, in int) {
		body := `{"type":"assistant","message":{"id":"` + id + `","model":"` + model + `","usage":{"input_tokens":` + string(rune('0'+in)) + `,"output_tokens":0}}}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(rootA, "projects", "p", "a.jsonl"), "a", "model-a", 1)
	write(filepath.Join(rootB, "projects", "p", "b.jsonl"), "b", "model-b", 2)
	s := newUsageScanner()
	a, err := s.Scan(filepath.Join(rootA, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Root != filepath.Join(rootA, "projects") || len(a.Models) != 1 || a.Models[0].Model != "model-a" {
		t.Fatalf("A report = %+v", a)
	}
	b, err := s.Scan(filepath.Join(rootB, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Root != filepath.Join(rootB, "projects") || len(b.Models) != 1 || b.Models[0].Model != "model-b" {
		t.Fatalf("B report = %+v", b)
	}
}

func TestUsageScannerDefaultSettingsPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects", "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","message":{"id":"d","model":"default-model","usage":{"input_tokens":3,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "projects", "p", "d.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sandboxDataDir(t)
	rep, err := newUsageScanner().Scan("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Root != filepath.Join(dir, "projects") || len(rep.Models) != 1 {
		t.Fatalf("default report = %+v", rep)
	}
}

func writeUsageLine(t *testing.T, path, id, model, timestamp string, in, out int) {
	t.Helper()
	line := `{"type":"assistant","timestamp":"` + timestamp + `","message":{"id":"` + id + `","model":"` + model + `","usage":{"input_tokens":` + string(rune('0'+in)) + `,"output_tokens":` + string(rune('0'+out)) + `}}}` + "\n"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestUsageTimestampNanoNoDateAndRangeInclusive(t *testing.T) {
	// Product dates are defined in time.Local; pin it so UTC fixtures have
	// deterministic calendar-day expectations on every machine.
	oldLocal := time.Local
	t.Cleanup(func() { time.Local = oldLocal })
	time.Local = time.UTC

	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	writeUsageLine(t, path, "early", "m", "2026-01-01T23:59:59.123456789Z", 1, 2)
	writeUsageLine(t, path, "early", "m", "2026-01-02T00:00:00.999999999Z", 9, 9)
	writeUsageLine(t, path, "middle", "m", "2026-01-02T12:00:00.000000001Z", 3, 4)
	writeUsageLine(t, path, "late", "m", "2026-01-03T00:00:00.000000001Z", 5, 6)
	writeUsageLine(t, path, "nodate", "m", "not-a-timestamp", 7, 8)
	settings := filepath.Join(dir, "settings.json")
	s := newUsageScanner()
	all, err := s.Scan(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.ByDay["2026-01-01"]) != 1 || len(all.ByDay["2026-01-02"]) != 1 || len(all.ByDay["2026-01-03"]) != 1 {
		t.Fatalf("byDay = %#v", all.ByDay)
	}
	if len(all.NoDate) != 1 || all.NoDate[0].Msgs != 1 {
		t.Fatalf("noDate = %#v", all.NoDate)
	}
	if all.Total.Msgs != 4 {
		t.Fatalf("all messages = %d, want 4", all.Total.Msgs)
	}
	if all.Total.In != 24 || all.Total.Out != 27 {
		t.Fatalf("all tokens = %d/%d, want 24/27", all.Total.In, all.Total.Out)
	}

	r, err := s.Scan(settings, nil, DateRange{From: "2026-01-02", To: "2026-01-02"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total.Msgs != 1 || r.Total.In != 3 || r.Total.Out != 4 {
		t.Fatalf("one-day range total = %+v", r.Total)
	}
	if len(r.ByDay) != 1 || len(r.ByDay["2026-01-02"]) != 1 {
		t.Fatalf("one-day byDay = %#v", r.ByDay)
	}
	if len(r.NoDate) != 1 {
		t.Fatalf("noDate metadata should remain visible: %#v", r.NoDate)
	}

	if _, err := s.Scan(settings, nil, DateRange{To: "2026-01-01"}); err == nil {
		t.Fatal("to-only range should be rejected")
	}
	if _, err := s.Scan(settings, nil, DateRange{From: "2026-01-03"}); err == nil {
		t.Fatal("from-only range should be rejected")
	}
}

func TestUsageSameMessageIDAcrossModelsRemainsDistinct(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	writeUsageLine(t, path, "same", "model-a", "2026-02-01T00:00:00Z", 1, 1)
	writeUsageLine(t, path, "same", "model-b", "2026-01-01T00:00:00Z", 8, 9)
	byDay, noDate, err := parseUsageFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(noDate) != 0 || len(byDay["2026-01-01"]) != 1 || len(byDay["2026-02-01"]) != 1 {
		t.Fatalf("model/id placement = %#v/%#v", byDay, noDate)
	}
	if a := byDay["2026-02-01"]["model-a"]; a == nil || a.Msgs != 1 || a.In != 1 || a.Out != 1 {
		t.Fatalf("model-a usage = %+v", a)
	}
	if b := byDay["2026-01-01"]["model-b"]; b == nil || b.Msgs != 1 || b.In != 8 || b.Out != 9 {
		t.Fatalf("model-b usage = %+v", b)
	}
}

func TestUsageDedupesModelMessageIDAcrossFiles(t *testing.T) {
	oldLocal := time.Local
	t.Cleanup(func() { time.Local = oldLocal })
	time.Local = time.UTC

	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(proj, "a.jsonl")
	b := filepath.Join(proj, "b.jsonl")
	writeUsageLine(t, a, "same", "model-a", "2026-02-02T00:00:00Z", 3, 7)
	writeUsageLine(t, b, "same", "model-a", "2026-01-01T00:00:00Z", 8, 4)

	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Msgs != 1 || rep.Total.In != 8 || rep.Total.Out != 7 {
		t.Fatalf("global max aggregate = %+v", rep.Total)
	}
	if len(rep.ByDay) != 1 || len(rep.ByDay["2026-01-01"]) != 1 {
		t.Fatalf("earliest timestamp placement = %#v", rep.ByDay)
	}
}

func TestUsageModelHintCanFollowUsageLine(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	body := `{"timestamp":"2026-01-01T00:00:00Z","message":{"id":"late-model","usage":{"input_tokens":10,"output_tokens":2}}}` + "\n" +
		`{"timestamp":"2026-01-01T00:00:01Z","message":{"id":"late-model","model":"model-a"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Msgs != 1 || rep.Total.In != 10 || rep.Total.Out != 2 || len(rep.Models) != 1 || rep.Models[0].Model != "model-a" {
		t.Fatalf("late model hint report = %+v", rep)
	}
}

func TestUsageModelOnlyHintContributesEarlierTimestamp(t *testing.T) {
	oldLocal := time.Local
	t.Cleanup(func() { time.Local = oldLocal })
	time.Local = time.UTC

	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	body := `{"timestamp":"2026-02-02T00:00:00Z","message":{"id":"hint-time","usage":{"input_tokens":10}}}` + "\n" +
		`{"timestamp":"2026-01-01T00:00:00Z","message":{"id":"hint-time","model":"model-a"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByDay) != 1 || len(rep.ByDay["2026-01-01"]) != 1 || rep.Total.In != 10 {
		t.Fatalf("model-only timestamp hint report = %+v", rep)
	}
}

func TestUsageAmbiguousModelHintDoesNotGuess(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	body := `{"message":{"id":"ambiguous","usage":{"input_tokens":10}}}` + "\n" +
		`{"message":{"id":"ambiguous","model":"model-a"}}` + "\n" +
		`{"message":{"id":"ambiguous","model":"model-b"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Msgs != 1 || rep.Total.In != 10 || len(rep.Models) != 1 || rep.Models[0].Model != "(未指定)" || rep.Models[0].Priced {
		t.Fatalf("ambiguous model usage must be preserved unpriced: %+v", rep)
	}
}

func TestUsageScannerCacheRejectsSameMetadataContentReplacement(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	first := `{"message":{"id":"one","model":"m","usage":{"input_tokens":1}}}` + "\n"
	second := `{"message":{"id":"two","model":"m","usage":{"input_tokens":9}}}` + "\n"
	if len(first) != len(second) {
		t.Fatal("fixture lengths differ")
	}
	if err := os.WriteFile(path, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newUsageScanner()
	if rep, err := s.Scan(filepath.Join(dir, "settings.json"), nil); err != nil || rep.Total.In != 1 {
		t.Fatalf("first scan = %+v, %v", rep, err)
	}
	if err := os.WriteFile(path, []byte(second), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if rep, err := s.Scan(filepath.Join(dir, "settings.json"), nil); err != nil || rep.Total.In != 9 {
		t.Fatalf("same-metadata replacement remained stale = %+v, %v", rep, err)
	}
}

func TestUsageMergeKeepsCoherentInputSnapshot(t *testing.T) {
	dst := map[usageKey]usageEntry{}
	key := usageKey{Model: "m", ID: "same"}
	mergeUsageEntry(dst, key, usageEntry{Usage: normalizedUsage{In: 100}})
	mergeUsageEntry(dst, key, usageEntry{Usage: normalizedUsage{In: 40, CacheW: 20, CacheR: 40, Out: 7}})
	got := dst[key].Usage
	if got.In != 100 || got.CacheW != 0 || got.CacheR != 0 || got.Out != 7 {
		t.Fatalf("mixed snapshots: %+v", got)
	}
}

func TestUsageScannerRecoversAfterOverlongJSONLRow(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	var b bytes.Buffer
	b.Write(bytes.Repeat([]byte{'x'}, (64<<20)+1))
	b.WriteByte('\n')
	b.WriteString(`{"message":{"id":"valid","model":"m","usage":{"input_tokens":9,"output_tokens":2}}}` + "\n")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Msgs != 1 || rep.Total.In != 9 || rep.Total.Out != 2 {
		t.Fatalf("valid row after overlong row lost: %+v", rep.Total)
	}
}

func TestUsageScannerCachesParseError(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, (64<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newUsageScanner()
	settings := filepath.Join(dir, "settings.json")
	if _, err := s.Scan(settings, nil); err == nil {
		t.Fatal("overlong unterminated row should propagate an error")
	}
	if _, err := s.Scan(settings, nil); err == nil {
		t.Fatal("cached scan error should propagate")
	}
}

func TestUsageScannerCacheRefreshesAfterAppend(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s.jsonl")
	writeUsageLine(t, path, "one", "m", "2026-01-01T00:00:00Z", 1, 1)
	settings := filepath.Join(dir, "settings.json")
	s := newUsageScanner()
	a, err := s.Scan(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Total.Msgs != 1 {
		t.Fatalf("first scan = %+v", a.Total)
	}
	time.Sleep(2 * time.Millisecond)
	writeUsageLine(t, path, "two", "m", "2026-01-02T00:00:00Z", 2, 2)
	b, err := s.Scan(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Total.Msgs != 2 || b.Total.In != 3 {
		t.Fatalf("cache did not refresh = %+v", b.Total)
	}
}
