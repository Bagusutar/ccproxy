package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// 524 在「响应头到达前」发生，客户端一个字节都没收到，重试是幂等安全的。
func TestRetryReplaysBodyWithoutGetBody(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := retryBackoff
	retryBackoff = []time.Duration{0, 0, 0}
	defer func() { retryBackoff = old }()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, io.NopCloser(strings.NewReader(`{"model":"m"}`)))
	req.GetBody = nil
	rt := &retryTransport{base: http.DefaultTransport.(*http.Transport).Clone(), logf: quietLogger()}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != `{"model":"m"}` || bodies[1] != bodies[0] {
		t.Fatalf("retry bodies = %q", bodies)
	}
}

func TestRetriesOn524BeforeAnyByteReachesClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(524)
			_, _ = io.WriteString(w, `{"error":"timeout"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	old := retryBackoff
	retryBackoff = []time.Duration{0, 0, 0}
	defer func() { retryBackoff = old }()

	rt := &retryTransport{base: http.DefaultTransport.(*http.Transport).Clone(), logf: quietLogger()}
	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader([]byte(`{"model":"x"}`)))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte(`{"model":"x"}`))), nil
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("状态码 = %d，应重试后拿到 200", resp.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("上游被调用 %d 次，应为 2（首次 524 + 一次重试）", got)
	}
}

// 配置类错误重试没有意义，只会把明确失败拖成漫长卡顿。
func TestDoesNotRetryPermanentErrors(t *testing.T) {
	for _, msg := range []string{
		`Post "": unsupported protocol scheme ""`,
		`parse "://x": missing protocol scheme`,
	} {
		if retryableErr(errString(msg)) {
			t.Errorf("不应重试: %s", msg)
		}
	}
	if !retryableErr(errString("read: connection reset by peer")) {
		t.Error("网络类错误应当重试")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// 4xx 是配置问题，重试无用。
func TestDoesNotRetry403(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":{"message":"This token has no access to model x","type":"new_api_error"}}`)
	}))
	defer srv.Close()

	rt := &retryTransport{base: http.DefaultTransport.(*http.Transport).Clone(), logf: quietLogger()}
	req, _ := http.NewRequest("POST", srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if hits.Load() != 1 {
		t.Errorf("403 被重试了 %d 次，应为 1 次即止", hits.Load())
	}
}

// 开流后静默必须提前收尾：等满客户端的 300s 会熬过 prompt cache 的 5 分钟 TTL，
// 导致重试时全量冷预填重新计费。
func TestStallGuardCutsStreamShort(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("event: message_start\ndata: {}\n\n"))
		// 之后永久静默，模拟链路中断
	}()

	g := newStallGuard(pr, 300*time.Millisecond, quietLogger())
	start := time.Now()
	out, err := io.ReadAll(g)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %v，应在静默超时后立即结束", elapsed)
	}
	if !strings.Contains(string(out), "message_start") {
		t.Error("已收到的上游数据被丢弃了")
	}
	if !strings.Contains(string(out), "overloaded_error") {
		t.Errorf("未注入 SSE 错误事件，客户端会以为流正常结束:\n%s", out)
	}
}

// 正常结束的流不应被打扰。
func TestStallGuardZeroLengthReadReturnsImmediately(t *testing.T) {
	r := &blockingReader{closed: make(chan struct{})}
	g := newStallGuard(r, time.Second, quietLogger())
	start := time.Now()
	if n, err := g.Read(make([]byte, 0)); n != 0 || err != nil {
		t.Fatalf("zero-length read = %d, %v", n, err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("zero-length read blocked for %v", elapsed)
	}
	_ = g.Close()
}

func TestStallGuardPassesThroughHealthyStream(t *testing.T) {
	body := "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n"
	g := newStallGuard(io.NopCloser(strings.NewReader(body)), 2*time.Second, quietLogger())
	out, err := io.ReadAll(g)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Errorf("正常流被改动了:\n got %q\nwant %q", out, body)
	}
	if strings.Contains(string(out), "overloaded_error") {
		t.Error("正常流被误判为静默")
	}
}

// 网关的 new_api_error 会被 Claude Code 渲染成「Please run /login」，
// 那是误导——真实原因是模型白名单，重新登录没有任何用。
func TestTranslates403IntoPermissionError(t *testing.T) {
	resp := &http.Response{
		StatusCode: 403,
		Header:     http.Header{},
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"code":"","message":"This token has no access to model claude-opus-5","type":"new_api_error"}}`)),
	}
	if err := translateGatewayError(resp); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	if !strings.Contains(s, "permission_error") {
		t.Errorf("未翻译成 permission_error: %s", s)
	}
	if !strings.Contains(s, "不是登录问题") {
		t.Errorf("未说明这不是登录问题: %s", s)
	}
	if !strings.Contains(s, "[1M]") {
		t.Errorf("未提示 [1M] 后缀陷阱: %s", s)
	}
}

// 非 403 响应必须原样放行。
func TestLeavesNon403Untouched(t *testing.T) {
	const body = `{"type":"message","content":[]}`
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if err := translateGatewayError(resp); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	if string(out) != body {
		t.Errorf("200 响应被改动: %s", out)
	}
}

func TestTranslate403PreservesOversizedBody(t *testing.T) {
	body := []byte(`{"error":{"message":"This token has no access to model x"}}` + strings.Repeat("x", 64<<10))
	resp := &http.Response{StatusCode: http.StatusForbidden, ContentLength: int64(len(body)), Header: http.Header{
		"Content-Length": []string{strconv.Itoa(len(body))},
	}, Body: io.NopCloser(bytes.NewReader(body))}
	if err := translateGatewayError(resp); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("oversized 403 changed/truncated: err=%v len=%d want=%d", err, len(got), len(body))
	}
	if resp.ContentLength != int64(len(body)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length changed: field=%d header=%q", resp.ContentLength, resp.Header.Get("Content-Length"))
	}
}

func TestTranslate403RestoresBodyAfterReadError(t *testing.T) {
	want := []byte(`{"error":{"message":"This token has no access to model x"}}`)
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: &errorAfterReader{data: want}}
	if err := translateGatewayError(resp); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, want) {
		t.Fatalf("read-error 403 changed body: %q", got)
	}
}

type errorAfterReader struct{ data []byte }

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}
func (r *errorAfterReader) Close() error { return nil }

func TestTranslate403GzipBodyIsHandledByProxyTransport(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		_, _ = gz.Write([]byte(`{"error":{"message":"This token has no access to model x"}}`))
		_ = gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(b.Bytes())
	}))
	defer upstream.Close()
	cfg := DefaultConfig()
	cfg.Providers[0].BaseURL = upstream.URL
	cfg.Providers[0].Token = "t"
	p := NewProxy(cfg, log.New(io.Discard, "", 0))
	front := httptest.NewServer(p)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !bytes.Contains(got, []byte("permission_error")) {
		t.Fatalf("gzip 403 not translated: status=%d body=%s hits=%d", resp.StatusCode, got, hits.Load())
	}
}

// A normal blocking reader must be closed when the stall timeout fires.
type blockingReader struct{ closed chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) { <-r.closed; return 0, io.EOF }
func (r *blockingReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

func TestStallGuardStopsPumpOnBlockingReader(t *testing.T) {
	r := &blockingReader{closed: make(chan struct{})}
	g := newStallGuard(r, 10*time.Millisecond, quietLogger())
	out, err := io.ReadAll(g)
	if err != nil || !bytes.Contains(out, []byte("overloaded_error")) {
		t.Fatalf("stall output err=%v body=%q", err, out)
	}
	select {
	case <-r.closed:
	case <-time.After(time.Second):
		t.Fatal("stall did not close blocking source")
	}
	select {
	case _, ok := <-g.ch:
		if ok {
			t.Fatal("pump did not stop")
		}
	case <-time.After(time.Second):
		t.Fatal("pump did not exit")
	}
}

type generatedHugeBody struct {
	prefix []byte
	left   int64
	closed atomic.Int32
}

func (r *generatedHugeBody) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.left)
	for i := int64(0); i < n; i++ {
		p[i] = ' '
	}
	r.left -= n
	return int(n), nil
}
func (r *generatedHugeBody) Close() error { r.closed.Add(1); return nil }

type stalledDiscardBody struct {
	closed chan struct{}
	once   sync.Once
}

func (b *stalledDiscardBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *stalledDiscardBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestDiscardedStalledResponseTimesOut(t *testing.T) {
	oldTimeout := discardedBodyDrainTimeout
	discardedBodyDrainTimeout = 40 * time.Millisecond
	t.Cleanup(func() { discardedBodyDrainTimeout = oldTimeout })

	body := &stalledDiscardBody{closed: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages", nil)
	done := make(chan struct{})
	go func() {
		(&retryTransport{}).recordDiscardedBody(req, body, -1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("discard drain remained blocked after timeout")
	}
}

func TestDiscardedHugeResponseIsIncrementallyMetered(t *testing.T) {
	meter := newUsageMeter()
	body := &generatedHugeBody{prefix: []byte(`{"usage":{"input_tokens":23,"output_tokens":5}}`), left: responseBodyCap + 1}
	expected := int64(len(body.prefix)) + body.left
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages", nil)
	tg := &target{id: "p", name: "provider", model: "m"}
	req = req.WithContext(context.WithValue(req.Context(), ctxTarget, tg))
	(&retryTransport{meter: meter, logf: quietLogger()}).recordDiscardedBody(req, body, expected)
	rows := meter.snapshot().Rows
	if len(rows) != 1 || rows[0].In != 23 || rows[0].Out != 5 || body.closed.Load() != 1 {
		t.Fatalf("huge discarded response = rows=%+v closes=%d", rows, body.closed.Load())
	}
}

func TestUsageFromBodyRejectsMalformedOrTruncated(t *testing.T) {
	for _, raw := range []string{`{"usage":{"input_tokens":10}`, `not-json`} {
		in, cw, cr, out := usageFromBody([]byte(raw))
		if in != 0 || cw != 0 || cr != 0 || out != 0 {
			t.Errorf("malformed usage counted: %q -> %d/%d/%d/%d", raw, in, cw, cr, out)
		}
	}
}

// 估算宁可高估：低估会让该压缩时不压缩，最终撞上下文溢出。
func TestTokenEstimateBiasesHigh(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"你好世界 hello world"}]}`)
	n := estimateTokens(body)
	// 真实值约 10–14，估算应明显高于它但不至于离谱。
	if n < 15 {
		t.Errorf("估算 %d 偏低，低估会导致上下文溢出", n)
	}
	if n > 200 {
		t.Errorf("估算 %d 过高，会导致过早压缩", n)
	}
}

// 工具定义与 tool_result 的内容都要计入，否则大工具集会被严重低估。
func TestTokenEstimateCountsToolsAndResults(t *testing.T) {
	plain := estimateTokens([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	withTools := estimateTokens([]byte(`{"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"read_file","description":"Read a file from disk and return its contents","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`))
	if withTools <= plain {
		t.Errorf("工具定义未计入: plain=%d withTools=%d", plain, withTools)
	}
}

// 上游的错误体必须原样交给客户端。曾经只留下用于打日志的前 512 字节，
// 而 Content-Length 头仍是原始长度——客户端按它等剩下的字节，
// 拿到的却是半截 JSON。翻译路径剥掉 Accept-Encoding 之后，
// 未压缩的错误体成了常态，这条路径也随之变得容易撞上。
func TestLongErrorBodyReachesClientIntact(t *testing.T) {
	long := `{"type":"error","error":{"type":"invalid_request_error","message":"` +
		strings.Repeat("x", 2000) + `"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, long)
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.Providers[0].BaseURL = upstream.URL
	cfg.Providers[0].Token = "t"
	p := NewProxy(cfg, log.New(io.Discard, "", 0))

	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != long {
		t.Fatalf("错误体被改动了：收到 %d 字节，期望 %d", len(got), len(long))
	}
}

// 「进程已创建」不等于「代理可用」。保存流程必须等到健康端点真的应答，
// 否则界面会在代理起来就死的情况下照样报告成功。
func TestPortFallbackWrapsAndFailsWithinValidRange(t *testing.T) {
	seen := []int{}
	got := findFreePortWith(65535, func(port int) bool {
		seen = append(seen, port)
		return port == defaultPort
	})
	if got != defaultPort || len(seen) != 2 || seen[0] != 65535 || seen[1] != defaultPort {
		t.Fatalf("wrap result=%d checked=%v", got, seen)
	}

	seen = nil
	got = findFreePortWith(65535, func(port int) bool {
		seen = append(seen, port)
		return false
	})
	if got != 0 || len(seen) != 50 {
		t.Fatalf("exhaustion result=%d checked=%d", got, len(seen))
	}
	for _, port := range seen {
		if !validPort(port) {
			t.Fatalf("checked invalid port %d", port)
		}
	}
}

func TestWaitDaemonReady(t *testing.T) {
	sandboxDataDir(t)
	const nonce = "ready-nonce"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__ccproxy/health" {
			_, _ = io.WriteString(w, `{"ok":true,"pid":4242,"nonce":"ready-nonce"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(&Status{PID: 4242, Port: port, Nonce: nonce, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := waitDaemonReady(port, 3*time.Second); err != nil {
		t.Fatalf("已在服务却判定失败: %v", err)
	}

	// 没人监听时必须在超时内返回错误，不能永远等下去。
	free := findFreePort(38000)
	start := time.Now()
	if err := waitDaemonReady(free, 600*time.Millisecond); err == nil {
		t.Fatal("端口无人监听，应当报错")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("超时控制失效，用了 %s", d)
	}
}

// 端口被占时应当如实返回 false，而不是让调用方去启动一个注定失败的进程。
func TestWaitPortFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if waitPortFree(port, 300*time.Millisecond) {
		t.Error("端口仍被占用，却报告已释放")
	}
	_ = ln.Close()
	if !waitPortFree(port, 3*time.Second) {
		t.Error("端口已释放，却报告仍被占用")
	}
}

// 重试表是「实证发生过才加」的：这里逐条钉住当前认可的集合，
// 免得日后有人顺手把整个 5xx 铺进来——那等于给不存在的故障扩 bug 面。
//
// 520 是 2026-08-11 17:08 实测补进来的：网关回过一次 CF 520
// （源站返回了无法解析的响应），1.3 秒就返回，当时不在表里于是原样
// 透传给了客户端，客户端自己退避了 60 秒。
func TestRetryableStatusSet(t *testing.T) {
	for _, code := range []int{408, 500, 502, 503, 504, 520, 524, 529} {
		if !retryableStatus(code) {
			t.Errorf("%d 应当重试", code)
		}
	}
	// 4xx 配置类错误重试没有意义；521-527 至今一次没出现过，不预先铺开。
	for _, code := range []int{200, 400, 401, 403, 404, 429, 521, 522, 523, 525} {
		if retryableStatus(code) {
			t.Errorf("%d 不该重试", code)
		}
	}
}
