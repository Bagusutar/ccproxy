package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// 本文件的每一条容错都对应 2026-08-06 对 246 个 session transcript 的穷举结果。
// 历史上从未真实发生过的错误（ECONNRESET / socket hang up / fetch failed /
// 502 / 503 / 429 / 500）刻意不做处理——为不存在的故障写分支只会增加 bug 面。

// 两个阈值的默认值见 DefaultConfig：
//
//	FirstByteSec=95 —— CF 的 Proxy Read Timeout 是 120s，抢在它之前动手，
//	  否则等来的是一个已经没救的 524（实测 4 次 524 均在 ~125.6s 返回）。
//	StallSec=60 —— 客户端默认阈值是 300s，而 prompt cache TTL 约 5 分钟，
//	  等满 300s 缓存必然过期、重试就要全量冷预填，压到 60s 才能省下这笔。
const maxAttempts = 3

// retryBackoff：首次立即重试（多为瞬时抖动），之后拉开间隔。
// 实证故障窗是分钟到小时级，窗口内密集重试无用，窗口外一次即恢复。
var retryBackoff = []time.Duration{0, 5 * time.Second, 20 * time.Second}

// retryableStatus 判断「响应头已到、但还没给客户端写任何字节」时是否值得重试。
// 此刻重试是幂等安全的。4xx 配置类错误重试没有意义。
//
// 520 是后补的：2026-08-11 17:08 实测网关回过一次 520（CF「源站返回了
// 无法解析的响应」），1.3 秒就返回，当时不在表里于是原样透传给了客户端，
// 客户端自己退避了 60 秒。只加这一个，不铺 521-527——那些至今一次没出现过。
func retryableStatus(code int) bool {
	switch code {
	case 408, 500, 502, 503, 504, 520, 524, 529:
		return true
	}
	return false
}

// retryTransport 在响应头到达之前做重试。
//
// 之所以放在 RoundTripper 而不是 handler 里：ReverseProxy 只有在
// RoundTrip 返回之后才会向客户端写第一个字节，所以这一层的重试
// 对客户端天然不可见，不存在重复内容的风险。
const responseBodyCap = 32 << 20

type retryTransport struct {
	base  *http.Transport
	logf  *log.Logger
	meter *usageMeter // 供补记补救路径里被丢弃的那一轮用量，可为 nil
}

// permanentErrHints 是重试没有意义的错误特征。
// url.Parse 几乎不拒绝任何输入，配置为空时错误会一路穿到传输层，
// 表现为 unsupported protocol scheme ""——这类错误重试一万次也一样。
var permanentErrHints = []string{
	"unsupported protocol scheme",
	"missing protocol scheme",
	"invalid URL",
	"no Host in request URL",
}

func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, hint := range permanentErrHints {
		if strings.Contains(s, hint) {
			return false
		}
	}
	return true
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.GetBody == nil {
		// RoundTripper callers are allowed to omit GetBody. Buffer once so every
		// retry sends the same POST body instead of reusing an exhausted stream.
		raw, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		}
	}

	return t.roundTripRetries(req, true)
}

// roundTripRetries applies retry policy to one logical request. Rewritten
// fallbacks disable fallback selection to avoid recursion while retaining retries.
func (t *retryTransport) roundTripRetries(req *http.Request, allowFallback bool) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if d := retryBackoff[attempt]; d > 0 {
			select {
			case <-time.After(d):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}

		r := req.Clone(req.Context())
		if req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = b
		}

		resp, err := t.base.RoundTrip(r)
		if err != nil {
			lastErr = err
			if !retryableErr(err) {
				return nil, err
			}
			if attempt+1 < maxAttempts {
				t.logf.Printf("retry %d/%d after transport error: %v", attempt+1, maxAttempts-1, err)
				continue
			}
			return nil, err
		}

		if retryableStatus(resp.StatusCode) && attempt+1 < maxAttempts {
			// This physical response is discarded regardless of size. Drain it through
			// the bounded incremental usage parser so a >32MiB body is still metered
			// without retaining the body.
			t.recordDiscardedBody(req, resp.Body, resp.ContentLength)
			lastErr = errors.New(resp.Status)
			t.logf.Printf("retry %d/%d after status %d", attempt+1, maxAttempts-1, resp.StatusCode)
			continue
		}

		// 结构化输出没生效时用强制工具调用补一次。放在这里而不是
		// retryableStatus：那个函数的语义是「原样重发」，而这里要改写请求。
		// 放在 RoundTrip 内同样对客户端不可见——ReverseProxy 要等
		// RoundTrip 返回才写第一个字节。
		// 两种形态：400（上游明确拒绝）与 200 但内容不合规（上游静默忽略）。
		if allowFallback && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusOK) {
			if fb := t.structuredFallback(req, resp); fb != nil {
				return fb, nil
			}
		}
		// 跨上游的 thinking 签名验不过时，剥掉这些块重发。
		// 排在 structured 之后：那条判据更窄（context 里有 plan 才成立），
		// 先让更确定的补救去试。
		if allowFallback && resp.StatusCode == http.StatusBadRequest {
			if fb := t.thinkingFallback(req, resp); fb != nil {
				return fb, nil
			}
		}

		return resp, nil
	}
	return nil, lastErr
}

// ---------- 流内静默看门狗 ----------

// sseStallEvent 是判定死流后注入客户端的事件。
// 用 overloaded_error 而非自定义类型，客户端已知如何处理它。
func sseStallEvent(d time.Duration) []byte {
	return []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error",` +
		`"message":"ccproxy: 上游已开流但静默超过 ` + itoa(int(d.Seconds())) +
		` 秒，判定为链路中断并提前结束。此时重试通常立即成功。"}}` + "\n\n")
}

// stallGuard 包裹上游响应体，在长时间无数据时提前收尾。
//
// 这类中断无法透明重试——客户端已经收到了 message_start 和若干 delta，
// 重发会产生重复内容，而 Anthropic 的 SSE 协议没有断点续传。
// 能做的只有尽早把控制权交还客户端，避免白等 300 秒把 prompt cache 熬过期。
type stallGuard struct {
	src     io.ReadCloser
	timeout time.Duration
	logf    *log.Logger

	ch         chan readResult
	stop       chan struct{}
	stopOnce   sync.Once
	closeOnce  sync.Once
	readMu     sync.Mutex
	closeErr   error
	started    bool
	tail       []byte // 注入的错误事件，待客户端读走
	pendingErr error  // Read returned bytes and an error; report error after bytes
	done       bool
}

type readResult struct {
	buf []byte
	err error
}

func newStallGuard(src io.ReadCloser, timeout time.Duration, logf *log.Logger) *stallGuard {
	return &stallGuard{src: src, timeout: timeout, logf: logf, ch: make(chan readResult, 1), stop: make(chan struct{})}
}

// pump 单独跑一个 goroutine 做阻塞读，主 Read 才能带超时等待。
func (g *stallGuard) pump() {
	defer close(g.ch)
	for {
		select {
		case <-g.stop:
			return
		default:
		}
		b := make([]byte, 32<<10)
		n, err := g.src.Read(b)
		if err != nil && n == 0 {
			// Preserve non-EOF failures even when no bytes accompanied them.
			// A clean EOF is represented by the pump closing its channel.
			if err == io.EOF {
				return
			}
			select {
			case g.ch <- readResult{err: err}:
			case <-g.stop:
				return
			}
			return
		}
		select {
		case <-g.stop:
			return
		default:
		}
		select {
		case g.ch <- readResult{buf: b[:n], err: err}:
		case <-g.stop:
			return
		}
		if err != nil {
			return
		}
	}
}

func (g *stallGuard) Read(p []byte) (int, error) {
	g.readMu.Lock()
	defer g.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(g.tail) > 0 {
		n := copy(p, g.tail)
		g.tail = g.tail[n:]
		return n, nil
	}
	if g.pendingErr != nil {
		err := g.pendingErr
		g.pendingErr = nil
		g.done = true
		return 0, err
	}
	if g.done {
		return 0, io.EOF
	}
	if !g.started {
		g.started = true
		go g.pump()
	}

	select {
	case res, ok := <-g.ch:
		if !ok {
			g.done = true
			return 0, io.EOF
		}
		if len(res.buf) > 0 {
			n := copy(p, res.buf)
			if n < len(res.buf) {
				// 缓冲区装不下时，剩余部分排到 tail 下次读走。
				g.tail = append([]byte(nil), res.buf[n:]...)
			}
			if res.err != nil {
				// io.Reader permits n > 0 together with an error. Deliver all
				// bytes first, then surface that error on the next Read.
				g.pendingErr = res.err
			}
			return n, nil
		}
		if res.err != nil {
			g.done = true
			return 0, res.err
		}
		return 0, nil

	case <-time.After(g.timeout):
		g.logf.Printf("stall detected: no upstream data for %s, cutting stream short", g.timeout)
		g.stopOnce.Do(func() { close(g.stop) })
		g.closeSource()
		g.done = true
		g.tail = append([]byte(nil), sseStallEvent(g.timeout)...)
		n := copy(p, g.tail)
		g.tail = g.tail[n:]
		return n, nil
	}
}

func (g *stallGuard) closeSource() {
	g.closeOnce.Do(func() { g.closeErr = g.src.Close() })
}

func (g *stallGuard) Close() error {
	g.stopOnce.Do(func() { close(g.stop) })
	g.closeSource()
	g.readMu.Lock()
	g.done = true
	g.readMu.Unlock()
	return g.closeErr
}

// ---------- 错误文案翻译 ----------

// translateGatewayError 把网关的 new_api_error 403 转成标准 Anthropic 错误结构。
//
// 网关原样返回时，Claude Code 会把 403 渲染成「Please run /login」，
// 误导用户去重新登录——实际是模型白名单或分组权限问题，重登没有任何用。
type gatewayUsageKey struct{}

func gatewayErrorUsage(resp *http.Response) (normalizedUsage, bool) {
	if resp == nil || resp.Request == nil {
		return normalizedUsage{}, false
	}
	u, ok := resp.Request.Context().Value(gatewayUsageKey{}).(normalizedUsage)
	return u, ok
}

// translateGatewayError inspects at most 64KiB+1 bytes. Non-matching or
// oversized bodies are restored byte-for-byte; only a complete matching JSON
// body is rewritten. The original usage is attached for one manual meter record.
func translateGatewayError(resp *http.Response) error {
	if resp.StatusCode != http.StatusForbidden {
		return nil
	}
	const capBytes = 64 << 10
	prefix, over, err := readBodyPrefix(resp.Body, capBytes)
	if err != nil {
		resp.Body = prependBody(prefix, resp.Body)
		return nil
	}
	if over {
		resp.Body = prependBody(prefix, resp.Body)
		return nil
	}
	// The body is complete and within the inspection cap. Invalid JSON and
	// unrelated 403s remain transparent, including their exact bytes.
	if !json.Valid(prefix) {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(prefix))
		return nil
	}
	msg := gjson.GetBytes(prefix, "error.message").String()
	if !strings.Contains(msg, "no access to model") {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(prefix))
		return nil
	}
	in, cw, cr, out := usageFromBody(prefix)
	if resp.Request != nil {
		resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), gatewayUsageKey{}, normalizedUsage{in, cw, cr, out}))
	}
	outBody, _ := json.Marshal(map[string]any{
		"type": "error", "error": map[string]string{
			"type":    "permission_error",
			"message": "网关拒绝了该模型：" + msg + " — 这不是登录问题，无需重新登录。原因通常是 token 模型白名单或账号分组权限，需要找网关管理员开通。另外请确认模型名不含 [1M] 后缀，网关只认裸名。",
		},
	})
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(outBody))
	resp.ContentLength = int64(len(outBody))
	resp.Header.Set("Content-Length", itoa(len(outBody)))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
	return nil
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ---------- count_tokens 本地兜底 ----------

// estimateTokens 在网关不支持 /v1/messages/count_tokens 时本地估算。
//
// 网关对该端点返回 {"error":{"message":"Invalid URL (POST /v1/messages/count_tokens)"}}，
// 而 Claude Code 会调它来判断何时压缩上下文。
//
// 刻意向上取整：低估会让该压缩时不压缩，最终撞上下文溢出直接报错；
// 高估只是提前压缩，代价小得多。所以 CJK 按 1 字符/token、
// 其余按 3.5 字符/token，并计入每条消息的框架开销。
const (
	tokensPerMessage = 8  // 角色标记、分隔符等
	tokensBaseline   = 20 // 请求级固定开销
)

func estimateTokens(body []byte) int {
	text := gjson.GetBytes(body, "system").String()
	messages := 0
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		messages++
		c := msg.Get("content")
		if c.Type == gjson.String {
			text += c.String()
			return true
		}
		c.ForEach(func(_, block gjson.Result) bool {
			// text / tool_use 的入参 / tool_result 的返回都占 token。
			text += block.Get("text").String()
			text += block.Get("input").Raw
			text += block.Get("content").Raw
			return true
		})
		return true
	})
	// 工具定义也计入。
	text += gjson.GetBytes(body, "tools").Raw

	var cjk, other int
	for _, r := range text {
		if r > 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	n := cjk + int(float64(other)/3.5) + messages*tokensPerMessage + tokensBaseline
	if n < 1 {
		n = 1
	}
	return n
}

// readBodyPrefix reads at most cap bytes plus one-byte overflow detection without
// consuming the remainder irreversibly; callers can prepend the prefix back.
func readBodyPrefix(r io.Reader, cap int) (prefix []byte, over bool, err error) {
	prefix, err = io.ReadAll(io.LimitReader(r, int64(cap)+1))
	if err != nil {
		return prefix, false, err
	}
	return prefix, len(prefix) > cap, nil
}

// recordDiscarded records usage from one physical response consumed internally.
func (t *retryTransport) recordDiscarded(req *http.Request, raw []byte) {
	if len(raw) == 0 {
		return
	}
	t.recordDiscardedBody(req, io.NopCloser(bytes.NewReader(raw)), int64(len(raw)))
}

var discardedBodyDrainTimeout = 5 * time.Second

func (t *retryTransport) recordDiscardedBody(req *http.Request, body io.ReadCloser, contentLength int64) {
	if body == nil {
		return
	}
	// Closing an HTTP response body unblocks its Read. Drain incrementally for
	// connection reuse and metering, but never wait forever on a stalled body.
	timer := time.AfterFunc(discardedBodyDrainTimeout, func() { _ = body.Close() })
	defer func() {
		timer.Stop()
		_ = body.Close()
	}()

	var reader io.Reader = body
	if t.meter != nil {
		if tg, _ := req.Context().Value(ctxTarget).(*target); tg != nil {
			reader = newUsageTap(body, false, func(in, cw, cr, out int64) {
				t.meter.Add(tg.id, tg.name, tg.meterModel(), in, cw, cr, out)
			}, contentLength)
		}
	}
	_, _ = io.Copy(io.Discard, reader)
}
