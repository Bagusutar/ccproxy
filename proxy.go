package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type ctxKey int

const (
	ctxTarget ctxKey = iota
	ctxStructured

	// requestBodyCap bounds the prompt retained in memory for routing, retries, and translation.
	requestBodyCap = 32 << 20
)

// target 是一次请求解析出的上游。
type target struct {
	name  string // 显示名，用于日志与错误提示
	id    string // Provider.ID
	how   string // 命中方式: slot / catalog / default
	url   *url.URL
	token string

	// xlate 非空表示这次要翻译，值是上游方言；为空则是透传快车道，
	// 响应一个字节都不碰。client 是客户端说的方言，决定翻回哪一种。
	xlate  Protocol
	client Protocol
	model  string // 供翻译回来时填 model 字段

	// wantJSON 表示这次请求带了 JSON Schema 约束。
	// 翻译时我们为了迁就 Responses 把可选属性改成了「必填 + 可为 null」，
	// 回程必须把那些 null 摘掉——见 stripNullFields。
	wantJSON  bool
	nullShape *structuredNullShape
}

// meterModel 是记账用的模型名。空名要有个占位，否则统计表里会出现一行没有
// 名字的数，看不出是谁花的。
func (t *target) meterModel() string {
	if t.model == "" {
		return "(未指定)"
	}
	return t.model
}

// Proxy 是常驻的路由代理。
type Proxy struct {
	mu   sync.RWMutex
	cfg  *Config
	rp   *httputil.ReverseProxy
	srv  *http.Server
	logf *log.Logger

	hits         sync.Map // Provider.ID -> *atomic.Uint64
	meter        *usageMeter
	startedAt    time.Time
	lastErr      atomic.Pointer[string]
	nonce        string
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

type healthIdentity struct {
	OK    bool   `json:"ok"`
	PID   int    `json:"pid"`
	Nonce string `json:"nonce"`
}

const shutdownNonceHeader = "X-CCProxy-Nonce"

func newDaemonNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func NewProxy(cfg *Config, logger *log.Logger) *Proxy {
	p := &Proxy{
		cfg:        cfg,
		logf:       logger,
		meter:      newUsageMeter(),
		startedAt:  time.Now(),
		nonce:      newDaemonNonce(),
		shutdownCh: make(chan struct{}),
	}
	if err := p.meter.Load(); err != nil {
		p.logf.Printf("usage meter load: %v", err)
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// 尊重 HTTP_PROXY / HTTPS_PROXY / NO_PROXY。
		//
		// Transport.Proxy 的零值是 nil，含义是「直连，不走任何代理」——
		// 在必须经企业代理或服务器代理才能出网的机器上，这等于连不上上游，
		// 而且症状是超时，看不出原因。ProxyFromEnvironment 在没设这些变量时
		// 是无操作，所以对绝大多数机器没有任何影响。
		//
		// 它自带 localhost 例外：发往 127.0.0.1 的请求不会被塞进代理。
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		// 只约束响应头到达时间；响应体（流式 token）不设上限，
		// 否则长响应会被代理自己掐断。
		// 95s 是刻意选的：抢在 Cloudflare 120s Proxy Read Timeout 之前动手，
		// 此时客户端一个字节都没收到，重发幂等安全。
		ResponseHeaderTimeout: time.Duration(cfg.FirstByteSec) * time.Second,
	}

	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t, _ := pr.In.Context().Value(ctxTarget).(*target)
			if t == nil {
				return
			}
			pr.Out.URL.Scheme = t.url.Scheme
			pr.Out.URL.Host = t.url.Host
			pr.Out.URL.Path = joinPath(t.url.Path, pr.In.URL.Path)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// 必须重设 Host，否则上游 SNI/虚拟主机匹配失败。
			pr.Out.Host = t.url.Host

			// 替换鉴权：客户端发来的是占位符，这里换成真实凭证。
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("X-Api-Key", t.token)
			pr.Out.Header.Set("Authorization", "Bearer "+t.token)

			// 一律不让上游按客户端的偏好压缩。
			//
			// Go 的 Transport 只在「自己加的 Accept-Encoding」时才透明解压；
			// 客户端一旦自带 br/zstd，到手的就是二进制，代理任何想读响应体的
			// 地方都会静默失灵。这一条踩过四次：翻译层解出空回复、
			// structured outputs 判据失效、错误体被截断、403 文案改写不生效
			// （日志里那些 [zstd-encoded body] 就是证据）。
			//
			// 曾经按需局部删，结果总有一处漏掉。改成无条件删：Go 会自己加
			// gzip 并解压，代价只是放弃 br/zstd 相对 gzip 那点额外压缩率，
			// 换来的是所有读响应体的路径一次性全对，日志也永远可读。
			pr.Out.Header.Del("Accept-Encoding")

			// 不向上游泄露本机代理链路信息。
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		// -1 表示每次写入立即 flush。这是流式透传的关键：
		// 任何缓冲都会让 SSE 攒到最后一次性吐出。
		FlushInterval:  -1,
		Transport:      &retryTransport{base: transport, logf: logger, meter: p.meter},
		ModifyResponse: p.onResponse,
		ErrorHandler:   p.onError,
	}
	return p
}

// onResponse 在响应头到达、正文尚未转发给客户端时介入。
func (p *Proxy) onResponse(resp *http.Response) error {
	// 记录上游状态码。ErrorHandler 只在传输层出错时触发，上游返回的
	// 4xx/5xx 会被 ReverseProxy 原样透传，不记在这里就完全不可见——
	// 排查时会看到「请求发出去了、没报错」，实际上游一直在拒。
	if resp.StatusCode >= 400 {
		name := "?"
		if t, ok := resp.Request.Context().Value(ctxTarget).(*target); ok && t != nil {
			name = t.name
		}
		// Upstream error bodies may echo prompts, credentials, or provider secrets.
		// Record only metadata; the client still receives the body unchanged.
		if enc := resp.Header.Get("Content-Encoding"); enc != "" {
			p.logf.Printf("upstream %d from %s (%s) [%s-encoded body]",
				resp.StatusCode, name, resp.Request.URL.Path, enc)
		} else {
			p.logf.Printf("upstream %d from %s (%s)",
				resp.StatusCode, name, resp.Request.URL.Path)
		}
	}

	// 403 inspection must precede the usage tap. A rewritten matching 403
	// records its original usage exactly once and never taps the old body.
	if err := translateGatewayError(resp); err != nil {
		return err
	}
	if t, ok := resp.Request.Context().Value(ctxTarget).(*target); ok && t != nil {
		if u, ok := gatewayErrorUsage(resp); ok {
			p.meter.Add(t.id, t.name, t.meterModel(), u.In, u.CacheW, u.CacheR, u.Out)
		}
		sse := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
		resp.Body = newUsageTap(resp.Body, sse,
			func(in, cw, cr, out int64) { p.meter.Add(t.id, t.name, t.meterModel(), in, cw, cr, out) }, resp.ContentLength)
	}

	// 翻译路径：把上游的 Responses 形态翻回 Anthropic 形态。
	// 此刻客户端还没收到任何字节，替换整个响应体是安全的。
	if t, ok := resp.Request.Context().Value(ctxTarget).(*target); ok &&
		t != nil && t.xlate == ProtoResponses && resp.StatusCode < 400 {
		return p.xlateResponses(resp, t)
	}

	// 只对流式响应加静默看门狗；非流式响应没有「开流后卡住」这个形态。
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		p.mu.RLock()
		d := time.Duration(p.cfg.StallSec) * time.Second
		p.mu.RUnlock()
		resp.Body = newStallGuard(resp.Body, d, p.logf)
	}
	return nil
}

// xlateResponses 把 Responses 响应换成 Anthropic 响应。
//
// 流式走增量翻译器，一个上游事件进、一个下游事件出，不攒包；
// 非流式一次性重写整个 body。两种情况都必须清掉 Content-Length，
// 翻译后长度必然变化，留着旧值客户端会读到截断的内容。
func (p *Proxy) xlateResponses(resp *http.Response, t *target) error {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		p.mu.RLock()
		d := time.Duration(p.cfg.StallSec) * time.Second
		p.mu.RUnlock()
		// 看门狗放在翻译器内侧：它守的是「上游开流后静默」，
		// 计时基准应该是上游的字节，不是翻译产出的字节。
		guarded := newStallGuard(resp.Body, d, p.logf)
		if t.client == ProtoChat {
			resp.Body = newResponsesChatStream(guarded, t.model, t.wantJSON, t.nullShape)
		} else {
			resp.Body = newResponsesStream(guarded, t.model, t.wantJSON, t.nullShape)
		}
		resp.Header.Del("Content-Length")
		resp.Header.Del("Content-Encoding")
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("读取上游响应失败: %w", err)
	}
	if len(raw) > 32<<20 {
		return fmt.Errorf("上游响应超过 32 MiB 限制")
	}
	xlateResp := responsesToAnthropic
	if t.client == ProtoChat {
		xlateResp = responsesToChat
	}
	out, err := xlateResp(raw, t.model)
	if err != nil {
		return fmt.Errorf("翻译上游响应失败: %w", err)
	}
	if t.wantJSON {
		out = stripNullFieldsWithShape(out, t.nullShape)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	// 上游可能压缩过，翻译后的是明文，旧的编码声明必须去掉。
	resp.Header.Del("Content-Encoding")
	return nil
}

// joinPath 拼接上游 base 路径与请求路径。
// DeepSeek 的 base 含 /anthropic，网关的 base 为空。
func joinPath(base, reqPath string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return reqPath
	}
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	return base + reqPath
}

func (p *Proxy) onError(w http.ResponseWriter, r *http.Request, err error) {
	msg := err.Error()
	p.lastErr.Store(&msg)
	p.logf.Printf("upstream error: %v (%s %s)", err, r.Method, r.URL.Path)

	// 原样以 Anthropic 错误结构返回，让 Claude Code 的重试逻辑能正确识别。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "api_error",
			"message": "ccproxy upstream error: " + msg,
		},
	})
}

// parseUpstream 校验并解析上游地址。
//
// url.Parse 对空串和缺 scheme 的输入都不报错，只会返回一个 Scheme/Host 为空的
// URL 对象，错误要到传输层才以 unsupported protocol scheme "" 的形式暴露出来。
// 在这里拦住，用户拿到的才是「上游未配置」这种能看懂的提示。
func parseUpstream(raw, label string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s 未配置地址，请在 ccproxy 界面中填写", label)
	}
	normalized, err := normalizeBaseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("%s 地址无效 (%s): %w", label, raw, err)
	}
	u, err := url.Parse(normalized)
	if err != nil { // normalizeBaseURL 已校验，这里仅保持返回类型安全
		return nil, err
	}
	return u, nil
}

// oneMSuffix 匹配 [1m] / [1M] 之类的上下文档位后缀。
// Claude Code 在发请求前就会把它转成 anthropic-beta 头，但别的客户端
// 可能原样发过来，而带后缀的模型名在上游一律是 403 / 404。
// 代理认得这个后缀，就该替它转换完——见 stripModelSuffix。
var oneMSuffix = regexp.MustCompile(`(?i)\[([0-9]+)m\]$`)

func bareModel(m string) string { return oneMSuffix.ReplaceAllString(strings.TrimSpace(m), "") }

// contextBetas 把档位后缀映射到上游认识的 beta 特性 id。
// 只列实际存在的档位——凭空拼一个 context-9m-… 只会换来另一种 400。
var contextBetas = map[string]string{
	"1": "context-1m-2025-08-07",
}

// stripModelSuffix 把请求体里的模型名换成裸名，并返回该后缀对应的 beta 特性。
//
// 光剥后缀就能拿到 200，但那是静默降级：用户要的是 1M 上下文，
// 拿到的是默认档位，且全程没有任何提示。补上 beta 头才是语义等价——
// 实测裸名 + context-1m-2025-08-07 返回 200，带后缀的名字返回 403。
//
// beta 为空表示这个档位没有已知的对应特性：此时仍然剥掉后缀，
// 因为带后缀的名字在上游必然失败，剥掉至少还有一半能用。
func stripModelSuffix(body []byte) (out []byte, beta string, changed bool) {
	raw := gjson.GetBytes(body, "model").String()
	m := oneMSuffix.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return body, "", false
	}
	nb, err := sjson.SetBytes(body, "model", bareModel(raw))
	if err != nil {
		return body, "", false
	}
	return nb, contextBetas[strings.ToLower(m[1])], true
}

// appendBeta 把一个 beta 特性并进 anthropic-beta 头。
// 用追加而非覆盖：客户端可能已经声明了别的特性，覆盖会把它们静默丢掉。
func appendBeta(h http.Header, beta string) {
	cur := strings.TrimSpace(h.Get("Anthropic-Beta"))
	if cur == "" {
		h.Set("Anthropic-Beta", beta)
		return
	}
	for _, part := range strings.Split(cur, ",") {
		if strings.EqualFold(strings.TrimSpace(part), beta) {
			return
		}
	}
	h.Set("Anthropic-Beta", cur+","+beta)
}

// resolve 决定请求走哪个上游。
//
// 查找顺序：槽位赋值 → 各上游探测到的模型列表 → 默认上游。
// 比前缀匹配可靠：subagent 在 frontmatter 里写任意模型名都能落到正确的上游。
//
// 选定上游后再决定用哪种方言发出去：模型原生支持客户端的方言就透传，
// 否则看有没有实现了的翻译方向。方言能力来自保存时的探测结论，
// 不在请求路径上现探。
func (p *Proxy) resolve(body []byte, reqPath string) (*target, error) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	model := ""
	if len(body) > 0 {
		model = bareModel(gjson.GetBytes(body, "model").String())
	}

	var prov *Provider
	var how string

	if model != "" {
		// 1. 槽位里显式指定过这个模型
		for _, s := range cfg.Slots {
			if s.Model != "" && strings.EqualFold(bareModel(s.Model), model) {
				if pv := cfg.ProviderByID(s.Provider); pv != nil {
					prov, how = pv, "slot"
					break
				}
			}
		}
		// 2. 某个上游探测到的模型列表里有它
		if prov == nil {
			for i := range cfg.Providers {
				for _, m := range cfg.Providers[i].Models {
					if strings.EqualFold(bareModel(m), model) {
						prov, how = &cfg.Providers[i], "catalog"
						break
					}
				}
				if prov != nil {
					break
				}
			}
		}
	}

	// 3. 兜底
	if prov == nil {
		prov, how = cfg.DefaultProvider(), "default"
	}
	if prov == nil {
		return nil, fmt.Errorf("尚未配置任何上游，请在 ccproxy 界面中添加")
	}

	label := prov.Name
	if label == "" {
		label = prov.ID
	}

	client := clientProtocol(reqPath)
	upstream, translate := pickUpstreamProtocol(prov, model, client)

	u, err := parseUpstream(prov.BaseForProtocol(upstream), label)
	if err != nil {
		return nil, err
	}
	if prov.Token == "" {
		return nil, fmt.Errorf("%s 未配置凭证，请在 ccproxy 界面中填写", label)
	}
	p.countHit(prov.ID)

	t := &target{name: label, id: prov.ID, how: how, url: u,
		token: prov.Token, model: model, client: client}
	if translate {
		t.xlate = upstream
	}
	return t, nil
}

// supportsCountTokens 查探测时记录的上游能力。
func (p *Proxy) supportsCountTokens(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if pv := p.cfg.ProviderByID(id); pv != nil {
		return pv.CountTokens
	}
	return false
}

func (p *Proxy) countHit(id string) {
	v, _ := p.hits.LoadOrStore(id, new(atomic.Uint64))
	v.(*atomic.Uint64).Add(1)
}

func (p *Proxy) hitSnapshot() map[string]uint64 {
	out := map[string]uint64{}
	p.hits.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return out
}

// versionlessEndpoints 是允许省略 /v1 版本段的已知端点。
//
// 起因是两个 SDK 的约定不同：Anthropic SDK 的 base URL 不含 /v1，
// 自己拼出 /v1/messages；OpenAI SDK 的 base URL 惯例上已经含 /v1，
// 只拼出 /chat/completions。同一个本地地址要同时服务两者，
// 就得把省略掉的那一段补回来。
//
// 刻意用精确匹配而非「不含 v1 就加」：代理的核心承诺是原样转发路径，
// 宽泛规则会把上游自定义的路径也一并改掉。
var versionlessEndpoints = map[string]bool{
	"/messages":              true,
	"/messages/count_tokens": true,
	"/chat/completions":      true,
	"/completions":           true,
	"/responses":             true,
	"/embeddings":            true,
	"/models":                true,
}

func normalizePath(p string) string {
	if versionlessEndpoints[p] {
		return "/v1" + p
	}
	return p
}

// isLocalOrigin 判断一个 Origin 头是否指向本机。
// 解析失败一律当成外部来源——拒错一个畸形的 Origin，代价只是少清一次统计。
func isLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// The proxy listener serves CLI/SDK clients, not the browser UI (which has its
// own random-port server), so any browser Origin is untrusted.
func crossSiteBrowserRequest(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("Origin")) != "" ||
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site")
}

func supportedProxyRequest(method, path string) bool {
	switch path {
	case "/v1/models":
		return method == http.MethodGet
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions",
		"/v1/completions", "/v1/responses", "/v1/embeddings":
		return method == http.MethodPost
	default:
		return false
	}
}

func (p *Proxy) serveShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	provided := strings.TrimSpace(r.Header.Get(shutdownNonceHeader))
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(p.nonce)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
	p.shutdownOnce.Do(func() { close(p.shutdownCh) })
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if crossSiteBrowserRequest(r) {
		p.logf.Printf("request rejected: explicit cross-site browser request (%s %s)", r.Method, r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// 重置累计用量。只接受 POST：GET 会被浏览器预取、被链接误触，
	// 而这是个有副作用的动作。
	if r.URL.Path == "/__ccproxy/reset-usage" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := p.meter.Reset(); err != nil {
			p.logf.Printf("usage meter reset failed: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		p.logf.Printf("usage meter reset")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}

	if r.URL.Path == "/__ccproxy/health" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(healthIdentity{OK: true, PID: os.Getpid(), Nonce: p.nonce})
		return
	}

	if r.URL.Path == "/__ccproxy/shutdown" {
		p.serveShutdown(w, r)
		return
	}

	// 补全省略的版本段，让 http://127.0.0.1:PORT 同时适用于
	// Anthropic 与 OpenAI 两种 base URL 约定，用户不必记住谁要带 /v1。
	r.URL.Path = normalizePath(r.URL.Path)
	reqPath := r.URL.Path
	// Reject before resolve/Rewrite can attach the provider credential.
	if !supportedProxyRequest(r.Method, reqPath) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 合并所有上游的模型目录后自行响应。
	// 配合 CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1，
	// Claude Code 的 /model 选择器就能直接列出全部上游的模型。
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models") {
		p.serveMergedModels(w)
		return
	}

	var body []byte
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		// 必须完整读入才能解析 model，同时限制内存占用；超限请求绝不触达上游。
		if r.ContentLength > requestBodyCap {
			_ = r.Body.Close()
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, requestBodyCap+1))
		_ = r.Body.Close()
		if err != nil {
			p.onError(w, r, fmt.Errorf("read request body: %w", err))
			return
		}
		if len(b) > requestBodyCap {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b

		// 档位后缀（claude-opus-5[1m]）在上游一律是 403/404。Claude Code 自己
		// 会先转换掉，但别的客户端可能原样发过来——代理既然认得这个后缀，
		// 就该替它转换完，而不是明知会失败还照发。
		if nb, beta, changed := stripModelSuffix(body); changed {
			body = nb
			if beta != "" {
				appendBeta(r.Header, beta)
			}
			p.logf.Printf("model suffix normalized: %q -> %q (beta=%q)",
				gjson.GetBytes(b, "model").String(),
				gjson.GetBytes(body, "model").String(), beta)
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		// 供 retryTransport 重放请求体。
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	t, err := p.resolve(body, r.URL.Path)
	if err != nil {
		p.onError(w, r, err)
		return
	}

	// 转发前的确定性清洗：空文本块无条件摘掉（对 Anthropic 永远非法），
	// thinking 块只摘该上游已经拒过的签名。能确定的事就别等上游来拒——
	// 那是一次注定失败的往返，延迟和配额都白花。
	if nb, changed := presanitize(body, t.id); changed {
		body = nb
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		r.Header.Del("Content-Length")
		p.logf.Printf("history sanitized: 摘掉了空文本块或已知被拒的 thinking 块 (%s)", t.name)
	}

	// structured outputs 的降级判定在这里做一次，结论随 context 传给传输层。
	// 放在传输层现算的话，每个请求都要为此复制一遍请求体——那可能是几 MB。
	// 翻译路径不参与：Responses 原生支持 schema，翻译器直接把它映射成
	// text.format。此处若也武断介入，看到的还是 Responses 形态的响应，
	// 只会误判成「不合规」并补发一个方言不对的请求。
	var plan *structuredPlan
	if t.xlate == "" {
		plan = planStructuredFallback(r.URL.Path, body)
	}

	// count_tokens 两种情况都本地估算：上游不支持这个端点（探测时已记录），
	// 或者这个模型要走翻译——Responses 没有对应端点，把一个计数请求
	// 翻成一次真实补全既荒谬又费钱。
	if strings.HasSuffix(r.URL.Path, "/v1/messages/count_tokens") &&
		(!p.supportsCountTokens(t.id) || t.xlate != "") {
		n := estimateTokens(body)
		p.logf.Printf("count_tokens served locally: %d (%s lacks this endpoint or needs translation)", n, t.name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": n})
		return
	}

	model := ""
	if len(body) > 0 {
		model = gjson.GetBytes(body, "model").String()
	}

	// 翻译路径：改写请求体与目标端点。透传路径完全不走这里。
	if t.xlate == ProtoResponses {
		// 在改写之前记下「这次带了 schema」：翻译会把可选属性改成
		// 必填加可为 null，回程要据此把 null 摘掉。
		schema := gjson.GetBytes(body, "output_config.format.schema")
		if !schema.Exists() {
			schema = gjson.GetBytes(body, "response_format.json_schema.schema")
		}
		t.wantJSON = schema.Exists()
		if t.wantJSON {
			t.nullShape = structuredNullShapeFromSchema(schema.Raw)
		}

		xlateReq := anthropicToResponses
		if t.client == ProtoChat {
			xlateReq = chatToResponses
		}
		nb, err := xlateReq(body)
		if err != nil {
			p.onError(w, r, fmt.Errorf("翻译请求失败: %w", err))
			return
		}
		body = nb
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		r.Header.Del("Content-Length")
		r.URL.Path = t.xlate.endpoint()
		p.logf.Printf("%s %s  model=%q -> %s (%s, 翻译 %s→%s)",
			r.Method, reqPath, model, t.name, t.how, t.client, t.xlate)
	} else {
		p.logf.Printf("%s %s  model=%q -> %s (%s)", r.Method, r.URL.Path, model, t.name, t.how)
	}

	ctx := context.WithValue(r.Context(), ctxTarget, t)
	if plan != nil {
		ctx = context.WithValue(ctx, ctxStructured, plan)
	}
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// serveMergedModels 返回所有上游模型的并集，去重后按名称排序。
func (p *Proxy) serveMergedModels(w http.ResponseWriter) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	seen := map[string]bool{}
	data := []map[string]any{}
	for _, pv := range cfg.Providers {
		for _, m := range pv.Models {
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			data = append(data, map[string]any{
				"id": m, "type": "model", "display_name": m,
			})
		}
	}
	sort.Slice(data, func(i, j int) bool {
		return data[i]["id"].(string) < data[j]["id"].(string)
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
}

// Reload 热替换配置，无需重启进程。
func (p *Proxy) Reload(cfg *Config) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	names := make([]string, 0, len(cfg.Providers))
	for _, pv := range cfg.Providers {
		names = append(names, fmt.Sprintf("%s(%d models)", pv.Name, len(pv.Models)))
	}
	p.logf.Printf("config reloaded: port=%d providers=%v", cfg.Port, names)
}

func (p *Proxy) newServer() *http.Server {
	return &http.Server{
		Handler: p,
		// 不设 WriteTimeout：流式响应可能持续数分钟。
		ReadHeaderTimeout: 30 * time.Second,
	}
}

func (p *Proxy) serve(srv *http.Server, ln net.Listener) error {
	err := srv.Serve(ln)
	p.mu.Lock()
	if p.srv == srv {
		p.srv = nil
	}
	p.mu.Unlock()
	return err
}

func (p *Proxy) start(port int, errCh chan<- error) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := p.newServer()
	p.mu.Lock()
	p.srv = srv
	p.mu.Unlock()
	p.logf.Printf("ccproxy listening on %s", addr)
	go func() { errCh <- p.serve(srv, ln) }()
	return nil
}

// Serve 在 127.0.0.1 上监听。绝不绑 0.0.0.0——内存里有两把 API key。
func (p *Proxy) Serve(port int) error {
	errCh := make(chan error, 1)
	if err := p.start(port, errCh); err != nil {
		return err
	}
	return <-errCh
}

func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.RLock()
	srv := p.srv
	p.mu.RUnlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func nextValidPort(port int) int {
	if port >= 65535 || port < 1 {
		return defaultPort
	}
	return port + 1
}

func findFreePortWith(start int, available func(int) bool) int {
	if !validPort(start) {
		start = defaultPort
	}
	port := start
	for range 50 {
		if available(port) {
			return port
		}
		port = nextValidPort(port)
	}
	return 0
}

// findFreePort 只检查合法端口；越过 65535 后从默认非特权端口继续。
func findFreePort(start int) int {
	return findFreePortWith(start, func(port int) bool {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return false
		}
		_ = ln.Close()
		return true
	})
}

// runDaemon 是 --daemon 模式入口：跑代理、监听配置变更、周期写状态。
//
// 外层循环用于「启动期参数」变更后整体重建。热重载只能替换配置对象，
// 改不了已经绑定的监听端口，也改不了构造 Transport 时就烧进
// ResponseHeaderTimeout 的首字节超时。不重建的话这两项会静默失效：
// 上游、模型分配、静默上限都更新了，端口和超时却还是旧的。
func runDaemon() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, daemonSignals()...)
	defer signal.Stop(sigCh)
	return runDaemonLoop(sigCh)
}

func runDaemonLoop(sigCh <-chan os.Signal) error {
	lp, err := logPath()
	if err != nil {
		return err
	}
	lf, err := openLogFile(lp)
	if err != nil {
		return err
	}
	defer lf.Close()
	logger := log.New(lf, "", log.LstdFlags|log.Lmicroseconds)

	for {
		cfg, err := LoadConfig()
		if err != nil {
			logger.Printf("load config: %v", err)
			return err
		}

		p := NewProxy(cfg, logger)
		ctx, cancel := context.WithCancel(context.Background())
		rebuild := make(chan struct{}, 1)
		errCh := make(chan error, 1)
		if err := p.start(cfg.Port, errCh); err != nil {
			cancel()
			return errors.Join(err, p.meter.Flush(), ClearStatus())
		}

		go watchConfig(ctx, p, cfg, rebuild)
		go heartbeat(ctx, p, cfg.Port)

		select {
		case err := <-errCh:
			cancel()
			return errors.Join(normalizeServeError(err), p.meter.Flush(), ClearStatus())

		case <-sigCh:
			cancel()
			return shutdownDaemon(p, errCh)

		case <-p.shutdownCh:
			cancel()
			return shutdownDaemon(p, errCh)

		case <-rebuild:
			logger.Printf("startup-time config changed (port / first-byte timeout), rebuilding")
			cancel()
			if err := shutdownDaemon(p, errCh); err != nil {
				return err
			}
			// 让出端口，避免新监听撞上 TIME_WAIT
			time.Sleep(300 * time.Millisecond)
		}
	}
}

// watchConfig 每 3 秒检查配置文件的 mtime。
//
// 上游、模型分配、静默上限热更新即可——它们每次请求现读。端口与首字节超时
// 不行：前者已经绑在监听套接字上，后者在 NewProxy 里进了 Transport。
// 这两项变了就通知外层整体重建，而不是让一半配置生效、另一半静默失效。
func normalizeServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func shutdownDaemonWithCleanup(p *Proxy, errCh <-chan error, cleanup func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	shutdownErr := p.Shutdown(ctx)
	cancel()
	var serveErr error
	select {
	case serveErr = <-errCh:
	case <-time.After(5 * time.Second):
		serveErr = fmt.Errorf("HTTP server did not stop after shutdown")
	}
	return errors.Join(shutdownErr, normalizeServeError(serveErr), p.meter.Flush(), cleanup())
}

func shutdownDaemon(p *Proxy, errCh <-chan error) error {
	return shutdownDaemonWithCleanup(p, errCh, ClearStatus)
}

func watchConfig(ctx context.Context, p *Proxy, cur *Config, rebuild chan<- struct{}) {
	cp, err := configPath()
	if err != nil {
		return
	}
	var last time.Time
	if st, err := os.Stat(cp); err == nil {
		last = st.ModTime()
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st, err := os.Stat(cp)
		if err != nil || !st.ModTime().After(last) {
			continue
		}
		last = st.ModTime()
		nc, err := LoadConfig()
		if err != nil {
			continue
		}
		if nc.Port != cur.Port || nc.FirstByteSec != cur.FirstByteSec {
			select {
			case rebuild <- struct{}{}:
			default:
			}
			return
		}
		p.Reload(nc)
	}
}

// heartbeat 周期写出运行状态；GUI 靠 status.json 的新鲜度判断 daemon 是否存活。
func heartbeat(ctx context.Context, p *Proxy, port int) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		s := &Status{
			PID:       os.Getpid(),
			Port:      port,
			Nonce:     p.nonce,
			StartedAt: p.startedAt,
			UpdatedAt: time.Now(),
			Hits:      p.hitSnapshot(),
		}
		if e := p.lastErr.Load(); e != nil {
			s.LastError = *e
		}
		_ = WriteStatus(s)
		if err := p.meter.Flush(); err != nil {
			p.logf.Printf("usage meter flush failed: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
