package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Anthropic 的流式响应把用量分两处送：message_start 带输入与缓存，
// message_delta 带最终输出（是累计值，不是增量）。两处都要捞到。
func TestScanSSEUsage(t *testing.T) {
	const body = `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"claude-opus-5","usage":{"input_tokens":12,"cache_creation_input_tokens":3400,"cache_read_input_tokens":58000,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":742}}

event: message_stop
data: {"type":"message_stop"}
`
	in, cw, cr, out := scanSSEUsage(body)
	if in != 12 || cw != 3400 || cr != 58000 {
		t.Errorf("输入侧 = %d/%d/%d，期望 12/3400/58000", in, cw, cr)
	}
	if out != 742 {
		t.Errorf("输出 = %d，期望 742（message_delta 的累计值，不是 message_start 里那个 1）", out)
	}
}

// tap 的第一职责是不影响正确性：字节必须原样透传，一个不多一个不少。
// 而且分块必须无关——真实的流会在任意位置断开。
func TestUsageTapPassesBytesThroughUnchanged(t *testing.T) {
	const body = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":900,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":33}}

`
	for _, chunk := range []int{1, 3, 17, 64, 4096} {
		var in, cw, cr, out int64
		tap := newUsageTap(io.NopCloser(&chunkedReader{s: body, n: chunk}), true,
			func(a, b, c, d int64) { in, cw, cr, out = a, b, c, d })
		got, err := io.ReadAll(tap)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("分块 %d：字节被改动了", chunk)
		}
		if in != 5 || cr != 900 || out != 33 || cw != 0 {
			t.Errorf("分块 %d：用量 = %d/%d/%d/%d，期望 5/0/900/33", chunk, in, cw, cr, out)
		}
	}
}

// 非流式响应：整个 body 是一个 JSON，读完再解析。
func TestUsageTapNonStreaming(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","content":[{"type":"text","text":"ok"}],
	  "usage":{"input_tokens":9,"cache_creation_input_tokens":120,"cache_read_input_tokens":30000,"output_tokens":58}}`
	var in, cw, cr, out int64
	tap := newUsageTap(io.NopCloser(strings.NewReader(body)), false,
		func(a, b, c, d int64) { in, cw, cr, out = a, b, c, d })
	got, _ := io.ReadAll(tap)
	if string(got) != body {
		t.Fatal("字节被改动了")
	}
	if in != 9 || cw != 120 || cr != 30000 || out != 58 {
		t.Errorf("用量 = %d/%d/%d/%d", in, cw, cr, out)
	}
}

// Responses 的口径不一样：input_tokens 含缓存，命中数在
// input_tokens_details.cached_tokens 里。要换算成 Anthropic 的口径，
// 否则同一张表里两种模型没法并排比。
func TestUsageTapNormalizesResponsesShape(t *testing.T) {
	const body = `{"object":"response","usage":{"input_tokens":50000,
	  "input_tokens_details":{"cached_tokens":48000},"output_tokens":220}}`
	var in, cw, cr, out int64
	tap := newUsageTap(io.NopCloser(strings.NewReader(body)), false,
		func(a, b, c, d int64) { in, cw, cr, out = a, b, c, d })
	_, _ = io.ReadAll(tap)
	if cr != 48000 {
		t.Errorf("缓存读 = %d，期望 48000", cr)
	}
	if in != 2000 {
		t.Errorf("非缓存输入 = %d，期望 2000（50000 总数减去 48000 命中）", in)
	}
	if out != 220 || cw != 0 {
		t.Errorf("输出/缓存写 = %d/%d", out, cw)
	}
}

func TestUsageTapClampsCacheSubsetsToInput(t *testing.T) {
	in, cw, cr, out := usageFromBody([]byte(`{"usage":{"input_tokens":10,
	  "input_tokens_details":{"cache_write_tokens":7,"cached_tokens":25},"output_tokens":3}}`))
	if in != 0 || cw != 7 || cr != 3 || out != 3 {
		t.Errorf("超总量缓存归一化 = %d/%d/%d/%d，期望 0/7/3/3", in, cw, cr, out)
	}
}

func TestUsageTapNormalizesResponsesCacheWriteAndRead(t *testing.T) {
	for _, c := range []struct {
		name, details   string
		in, cw, cr, out int64
	}{
		{"首次写入", `"cache_write_tokens":1109,"cached_tokens":0`, 3, 1109, 0, 7},
		{"后续读取", `"cache_write_tokens":0,"cached_tokens":1109`, 3, 0, 1109, 7},
	} {
		body := []byte(`{"usage":{"input_tokens":1112,"input_tokens_details":{` + c.details + `},"output_tokens":7}}`)
		in, cw, cr, out := usageFromBody(body)
		if in != c.in || cw != c.cw || cr != c.cr || out != c.out {
			t.Errorf("%s = %d/%d/%d/%d，期望 %d/%d/%d/%d", c.name,
				in, cw, cr, out, c.in, c.cw, c.cr, c.out)
		}
	}
}

func TestUsageTapMergesResponsesCacheAndOutputAcrossSSE(t *testing.T) {
	const body = `event: response.created
data: {"response":{"usage":{"input_tokens":100,"output_tokens":1}}}

event: response.completed
data: {"response":{"usage":{"input_tokens":100,"input_tokens_details":{"cache_write_tokens":10,"cached_tokens":40},"output_tokens":7}}}

`
	in, cw, cr, out := scanSSEUsage(body)
	if in != 50 || cw != 10 || cr != 40 || out != 7 {
		t.Errorf("Responses SSE 用量 = %d/%d/%d/%d，期望 50/10/40/7", in, cw, cr, out)
	}
}

// 累加器只在有变化时才落盘，且零用量不记账。
func TestMeterSkipsEmpty(t *testing.T) {
	m := newUsageMeter()
	m.Add("p1", "网关", "claude-opus-5", 0, 0, 0, 0)
	if len(m.snapshot().Rows) != 0 {
		t.Error("零用量不该记账——上游报错时本来就没有 usage")
	}
	m.Add("p1", "网关", "claude-opus-5", 1, 2, 3, 4)
	m.Add("p1", "网关", "claude-opus-5", 10, 20, 30, 40)
	rows := m.snapshot().Rows
	if len(rows) != 1 || rows[0].Reqs != 2 || rows[0].In != 11 || rows[0].Out != 44 {
		t.Errorf("累加不对: %+v", rows)
	}
	if rows[0].Name != "网关" {
		t.Errorf("上游显示名没带上: %q", rows[0].Name)
	}
}

func TestMeterFlushFailureKeepsDirtyAndRetries(t *testing.T) {
	sandboxDataDir(t)
	m := newUsageMeter()
	calls := 0
	m.write = func(string, []byte, os.FileMode) error {
		calls++
		if calls == 1 {
			return io.ErrClosedPipe
		}
		return nil
	}
	m.Add("p", "P", "m", 1, 0, 0, 2)
	if err := m.Flush(); err == nil {
		t.Fatal("flush failure was not returned")
	}
	if !m.dirty {
		t.Fatal("写失败后 dirty 被错误清除")
	}
	m.Flush()
	if m.dirty || calls != 2 {
		t.Fatalf("重试状态 = dirty:%v calls:%d", m.dirty, calls)
	}
}

func TestMeterResetWriteFailurePreservesOldFileAndRetries(t *testing.T) {
	sandboxDataDir(t)
	m := newUsageMeter()
	m.Add("p", "P", "m", 5, 0, 0, 6)
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	old := ReadMeter()
	m.write = func(string, []byte, os.FileMode) error { return io.ErrClosedPipe }
	if err := m.Reset(); err == nil {
		t.Fatal("reset write failure was not returned")
	}
	if !m.dirty {
		t.Fatal("failed reset must remain dirty")
	}
	if got := ReadMeter(); len(got.Rows) != len(old.Rows) || got.Rows[0].In != old.Rows[0].In {
		t.Fatalf("failed reset changed or lost old file: got=%+v old=%+v", got.Rows, old.Rows)
	}
	m.write = atomicWrite
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if m.dirty || len(ReadMeter().Rows) != 0 {
		t.Fatal("successful retry did not persist cleared meter")
	}
}

func TestMeterFlushConcurrentAddKeepsDirty(t *testing.T) {
	sandboxDataDir(t)
	m := newUsageMeter()
	started := make(chan struct{})
	release := make(chan struct{})
	m.write = func(string, []byte, os.FileMode) error {
		close(started)
		<-release
		return nil
	}
	m.Add("p", "P", "m", 1, 0, 0, 2)
	done := make(chan struct{})
	go func() { m.Flush(); close(done) }()
	<-started
	m.Add("p", "P", "m", 3, 0, 0, 4)
	close(release)
	<-done
	if !m.dirty {
		t.Fatal("并发 Add 后 dirty 丢失")
	}
	m.write = func(string, []byte, os.FileMode) error { return nil }
	m.Flush()
	if m.dirty {
		t.Fatal("第二次 Flush 未清 dirty")
	}
	if got := m.snapshot().Rows[0]; got.Reqs != 2 || got.In != 4 || got.Out != 6 {
		t.Fatalf("并发累加丢失: %+v", got)
	}
}

func TestUsageTapSSELineCapAndUnterminatedLine(t *testing.T) {
	long := strings.Repeat("x", tapSSELineCap+1024)
	body := "data: " + long + "\n\ndata: {\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}"
	var in, out int64
	tap := newUsageTap(io.NopCloser(strings.NewReader(body)), true,
		func(a, _, _, d int64) { in, out = a, d })
	got, err := io.ReadAll(tap)
	if err != nil || string(got) != body {
		t.Fatalf("超长 SSE 未原样透传: err=%v len=%d/%d", err, len(got), len(body))
	}
	if in != 9 || out != 2 || tap.line.Cap() > tapSSELineCap {
		t.Fatalf("超长 SSE 应丢当前行但恢复后续行/内存错误: %d/%d cap=%d", in, out, tap.line.Cap())
	}

	const final = "data: {\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}"
	var finIn, finOut int64
	finalTap := newUsageTap(io.NopCloser(strings.NewReader(final)), true,
		func(a, _, _, d int64) { finIn, finOut = a, d })
	if _, err := io.ReadAll(finalTap); err != nil {
		t.Fatal(err)
	}
	if finIn != 7 || finOut != 3 {
		t.Fatalf("EOF 无换行行未解析: %d/%d", finIn, finOut)
	}
}

func TestUsageTapSSEDispatchesFinalEventAtCleanEOF(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"line newline no blank", "data: {\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}\n"},
		{"line without newline", "data: {\"usage\":{\"input_tokens\":11,\"output_tokens\":5}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []int64
			tap := newUsageTap(io.NopCloser(strings.NewReader(tc.body)), true,
				func(in, cw, cr, out int64) { got = []int64{in, cw, cr, out} })
			if raw, err := io.ReadAll(tap); err != nil || string(raw) != tc.body {
				t.Fatalf("透传 = %q/%v", raw, err)
			}
			if want := []int64{tcExpectedInput(tc.name), 0, 0, tcExpectedOutput(tc.name)}; !equalInt64s(got, want) {
				t.Fatalf("usage = %v, want %v", got, want)
			}
		})
	}
}

func tcExpectedInput(name string) int64 {
	if name == "line without newline" {
		return 11
	}
	return 7
}
func tcExpectedOutput(name string) int64 {
	if name == "line without newline" {
		return 5
	}
	return 3
}
func TestUsageTapContentLengthCompletesWithoutEOF(t *testing.T) {
	body := `{"usage":{"input_tokens":17,"output_tokens":4}}`
	var got []int64
	tap := newUsageTap(io.NopCloser(strings.NewReader(body)), false,
		func(in, cw, cr, out int64) { got = []int64{in, cw, cr, out} }, int64(len(body)))
	buf := make([]byte, len(body))
	if n, err := io.ReadFull(tap, buf); err != nil || n != len(body) {
		t.Fatalf("read = %d/%v", n, err)
	}
	if err := tap.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []int64{17, 0, 0, 4}; !equalInt64s(got, want) {
		t.Fatalf("usage = %v, want %v", got, want)
	}
}

type tapBlockingReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *tapBlockingReader) Read([]byte) (int, error) { <-r.closed; return 0, io.EOF }
func (r *tapBlockingReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestUsageTapConcurrentCloseUnblocksRead(t *testing.T) {
	src := &tapBlockingReader{closed: make(chan struct{})}
	tap := newUsageTap(src, false, nil, 1)
	done := make(chan struct{})
	go func() {
		_, _ = tap.Read(make([]byte, 1))
		close(done)
	}()
	if err := tap.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock concurrent Read")
	}
}

func TestUsageTapTruncatedContentLengthDoesNotCommit(t *testing.T) {
	body := `{"usage":{"input_tokens":17,"output_tokens":4}}`
	var reports int
	tap := newUsageTap(io.NopCloser(strings.NewReader(body)), false,
		func(_, _, _, _ int64) { reports++ }, int64(len(body)+1))
	_, _ = io.ReadAll(tap)
	_ = tap.Close()
	if reports != 1 || tap.in != 0 || tap.out != 0 {
		t.Fatalf("truncated response committed: reports=%d usage=%d/%d", reports, tap.in, tap.out)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestUsageTapSSEReadErrorDropsOnlyIncompleteEvent(t *testing.T) {
	const complete = "data: {\"usage\":{\"input_tokens\":13,\"output_tokens\":2}}\n\n"
	for _, tc := range []struct {
		name string
		body string
		want []int64
	}{
		{"complete then error", complete + "data: {\"usage\":{\"input_tokens\":99", []int64{13, 0, 0, 2}},
		{"incomplete only", "data: {\"usage\":{\"input_tokens\":99", []int64{0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []int64
			tap := newUsageTap(&errorReader{body: tc.body, err: io.ErrUnexpectedEOF}, true,
				func(in, cw, cr, out int64) { got = []int64{in, cw, cr, out} })
			if raw, err := io.ReadAll(tap); err != io.ErrUnexpectedEOF || string(raw) != tc.body {
				t.Fatalf("read = %q/%v", raw, err)
			}
			if !equalInt64s(got, tc.want) {
				t.Fatalf("usage = %v, want %v", got, tc.want)
			}
		})
	}
}

// errorReader returns all bytes together with a non-EOF transport error.
type errorReader struct {
	body string
	err  error
	done bool
}

func (r *errorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.body), r.err
}
func (r *errorReader) Close() error { return nil }

// chunkedReader 按固定大小切块，用来验证跨块的行重组。
type chunkedReader struct {
	s string
	n int
	i int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	end := min(r.i+min(r.n, len(p)), len(r.s))
	n := copy(p, r.s[r.i:end])
	r.i += n
	return n, nil
}

// 重置必须由 daemon 自己做：计数活在内存里，外部删掉 usage.json
// 只会被下一次心跳原样写回去。这条守的是那个坑。
func TestMeterResetClearsMemoryNotJustFile(t *testing.T) {
	sandboxDataDir(t)
	m := newUsageMeter()
	m.Add("p1", "网关", "claude-opus-5", 100, 200, 300, 400)
	m.Flush()

	if got := ReadMeter(); len(got.Rows) != 1 {
		t.Fatalf("落盘失败: %+v", got)
	}
	// 比底层的 time.Time 而不是格式化后的字符串：RFC3339 只到秒，
	// 而这个测试整体跑完不到一毫秒，两个时间戳会格式化成同一秒。
	before := m.since

	m.Reset()
	if rows := m.snapshot().Rows; len(rows) != 0 {
		t.Errorf("内存没清干净: %+v", rows)
	}
	if got := ReadMeter(); len(got.Rows) != 0 {
		t.Errorf("文件没清干净: %+v", got.Rows)
	}
	if !m.since.After(before) {
		t.Error("起始时间应当重新开始计")
	}

	// 重置之后还能继续记，且不带上旧数
	m.Add("p1", "网关", "claude-opus-5", 1, 0, 0, 2)
	m.Flush()
	rows := ReadMeter().Rows
	if len(rows) != 1 || rows[0].In != 1 || rows[0].Out != 2 || rows[0].Reqs != 1 {
		t.Errorf("重置后的累加不对: %+v", rows)
	}
}

// 补救路径会在传输层内部丢掉一整轮已经付过钱的响应（200 但内容不合规
// = 上游完整推理过一遍）。tap 挂在 ModifyResponse，看不见它，
// 只能靠这条把用量捞回来。
func TestUsageFromBody(t *testing.T) {
	in, cw, cr, out := usageFromBody([]byte(`{"id":"msg_1","content":[],
	  "usage":{"input_tokens":7,"cache_creation_input_tokens":11,
	  "cache_read_input_tokens":900,"output_tokens":42}}`))
	if in != 7 || cw != 11 || cr != 900 || out != 42 {
		t.Errorf("用量 = %d/%d/%d/%d，期望 7/11/900/42", in, cw, cr, out)
	}
	// 读不出用量时必须是零，不能瞎猜——补记不到账好过记错账
	if a, b, c, d := usageFromBody([]byte(`{"error":"nope"}`)); a|b|c|d != 0 {
		t.Errorf("无 usage 的响应体不该产生数字: %d/%d/%d/%d", a, b, c, d)
	}
}

// reset 是个有副作用的端点。光挡 method 拦不住网页：一个自动提交的
// <form method=POST> 属于 CORS「简单请求」，不触发预检，浏览器会真发出去，
// 于是任意站点都能替用户清掉统计。
func TestResetUsageRejectsCrossOrigin(t *testing.T) {
	sandboxDataDir(t)
	p := NewProxy(DefaultConfig(), log.New(io.Discard, "", 0))

	call := func(method, origin string) int {
		r := httptest.NewRequest(method, "/__ccproxy/reset-usage", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w.Code
	}

	p.meter.Add("p1", "网关", "m", 1, 0, 0, 2)
	if got := call(http.MethodGet, ""); got != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d，应当 405", got)
	}
	if got := call(http.MethodPost, "https://evil.example"); got != http.StatusForbidden {
		t.Errorf("跨站 POST = %d，应当 403", got)
	}
	if len(p.meter.snapshot().Rows) != 1 {
		t.Error("被拒的请求把数据清掉了")
	}

	// 面板由独立的随机端口服务器承载；credential-bearing 代理端口不服务
	// 浏览器 UI。因此任何非空 Origin（包括本机 Origin）都不可信，只有 Go
	// 侧不带 Origin 的请求可以执行重置。
	if got := call(http.MethodPost, ""); got != http.StatusOK {
		t.Errorf("无 Origin 的 POST = %d，应当放行", got)
	}
	if len(p.meter.snapshot().Rows) != 0 {
		t.Error("放行的请求没真的清掉数据")
	}
	p.meter.Add("p1", "网关", "m", 1, 0, 0, 2)
	if got := call(http.MethodPost, "http://127.0.0.1:15722"); got != http.StatusForbidden {
		t.Errorf("本机 Origin 的 POST = %d，应当拒绝", got)
	}
	if len(p.meter.snapshot().Rows) != 1 {
		t.Error("被拒的本机来源请求把数据清掉了")
	}
}

// 出网必须经企业/服务器代理的机器上，Transport.Proxy 为 nil 就等于连不上上游，
// 而症状只是超时，完全看不出原因。
//
// 只断言这个字段被设上了，不去验环境变量的解析结果：Go 的
// ProxyFromEnvironment 在进程内用 sync.Once 缓存了一次读到的环境，
// 测试里 t.Setenv 未必生效，断言会随测试执行顺序飘。
func TestUpstreamTransportHonorsProxyEnv(t *testing.T) {
	sandboxDataDir(t)
	p := NewProxy(DefaultConfig(), log.New(io.Discard, "", 0))
	rt, ok := p.rp.Transport.(*retryTransport)
	if !ok {
		t.Fatalf("Transport 类型变了: %T", p.rp.Transport)
	}
	if rt.base.Proxy == nil {
		t.Error("Transport.Proxy 为 nil —— 必须经代理出网的机器上连不到上游")
	}
}

func TestDateRangeValidationAndInclusiveBounds(t *testing.T) {
	cases := []struct {
		name string
		r    DateRange
		ok   bool
	}{
		{"empty", DateRange{}, true},
		{"both valid", DateRange{"2026-01-02", "2026-01-03"}, true},
		{"same day", DateRange{"2026-01-02", "2026-01-02"}, true},
		{"from only", DateRange{From: "2026-01-02"}, false},
		{"to only", DateRange{To: "2026-01-02"}, false},
		{"bad month", DateRange{"2026-13-01", "2026-13-02"}, false},
		{"bad day", DateRange{"2026-02-30", "2026-03-01"}, false},
		{"bad shape", DateRange{"2026-1-01", "2026-01-02"}, false},
		{"reverse", DateRange{"2026-01-03", "2026-01-02"}, false},
		{"only empty from", DateRange{From: "", To: "2026-01-02"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.Valid(); got != tc.ok {
				t.Fatalf("Valid() = %v, want %v", got, tc.ok)
			}
		})
	}
	for _, tc := range []struct {
		r    DateRange
		day  string
		want bool
	}{
		{DateRange{"2026-01-02", "2026-01-03"}, "2026-01-02", true},
		{DateRange{"2026-01-02", "2026-01-03"}, "2026-01-03", true},
		{DateRange{From: "2026-01-02"}, "2026-12-31", false},
		{DateRange{To: "2026-01-02"}, "2025-12-31", false},
		{DateRange{"2026-01-02", "2026-01-03"}, "2026-01-04", false},
	} {
		if got := tc.r.Includes(tc.day); got != tc.want {
			t.Errorf("%+v Includes(%q) = %v, want %v", tc.r, tc.day, got, tc.want)
		}
	}
}

func TestMeterFileStrictValidationAndInvalidLoadClears(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC).Format(time.RFC3339Nano)
	base := func() map[string]any {
		return map[string]any{"version": 1, "since": now, "days": map[string]any{
			"2026-01-02": []any{map[string]any{"provider": "p", "model": "m", "reqs": 1, "in": 2, "cacheW": 3, "cacheR": 4, "out": 5}},
		}}
	}
	valid, _ := json.Marshal(base())
	if _, err := validateMeterFile(valid); err != nil {
		t.Fatalf("valid meter rejected: %v", err)
	}
	mutations := []struct {
		name string
		fn   func(map[string]any)
	}{
		{"legacy rows", func(v map[string]any) { delete(v, "days"); v["rows"] = []any{} }},
		{"unknown top field", func(v map[string]any) { v["extra"] = true }},
		{"invalid day", func(v map[string]any) { v["days"].(map[string]any)["2026-02-30"] = []any{} }},
		{"negative", func(v map[string]any) {
			v["days"].(map[string]any)["2026-01-02"].([]any)[0].(map[string]any)["out"] = -1
		}},
		{"duplicate", func(v map[string]any) {
			rows := v["days"].(map[string]any)["2026-01-02"].([]any)
			v["days"].(map[string]any)["2026-01-02"] = []any{rows[0], rows[0]}
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.fn(v)
			raw, _ := json.Marshal(v)
			if _, err := validateMeterFile(raw); err == nil {
				t.Fatal("invalid meter accepted")
			}
		})
	}

	sandboxDataDir(t)
	m := newUsageMeter()
	m.Add("p", "P", "m", 1, 0, 0, 1)
	m.Flush()
	p, err := meterPath()
	if err != nil {
		t.Fatal(err)
	}
	bad := append(valid[:len(valid)-1], []byte(`,"unknown": true}`)...)
	if err := os.WriteFile(p, bad, 0600); err != nil {
		t.Fatal(err)
	}
	m.Load()
	if got := len(m.snapshot().Rows); got != 0 {
		t.Fatalf("invalid load retained %d rows", got)
	}
	if got := len(ReadMeter().Rows); got != 0 {
		t.Fatalf("invalid read retained %d rows", got)
	}

}

func TestMeterAddAtJapaneseLocalDaySnapshotReadRangeAndRoundTrip(t *testing.T) {
	old := time.Local
	t.Cleanup(func() { time.Local = old })
	time.Local, _ = time.LoadLocation("Asia/Tokyo")
	sandboxDataDir(t)
	m := newUsageMeter()
	m.AddAt(time.Date(2026, 1, 1, 14, 59, 0, 0, time.UTC), "p", "P", "m", 1, 2, 3, 4)
	m.AddAt(time.Date(2026, 1, 1, 15, 1, 0, 0, time.UTC), "p", "P", "m", 10, 20, 30, 40)
	f := m.snapshot()
	if len(f.Days) != 2 || len(f.Days["2026-01-02"]) != 1 || len(f.Days["2026-01-01"]) != 1 {
		t.Fatalf("local-day split = %#v", f.Days)
	}
	m.Flush()
	m2 := newUsageMeter()
	m2.Load()
	if got := m2.snapshot(); len(got.Days) != 2 || got.Days["2026-01-02"][0].In != 10 {
		t.Fatalf("roundtrip = %#v", got.Days)
	}
	m2.Reset()
	if len(m2.snapshot().Days) != 0 || len(ReadMeter().Days) != 0 {
		t.Fatal("reset did not clear meter")
	}
}
