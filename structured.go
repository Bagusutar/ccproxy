package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 本文件用工具调用模拟 Anthropic 的 structured outputs。
//
// 起因：部分中转网关对带 output_config.format 的请求一律回
// 「400 structured_outputs not supported in your workspace」，
// 而 Claude Code 用这个特性做 goal 评估、hook 判定、会话标题生成等等，
// 报错会直接冒到界面上。实测 246 个 transcript 里有 36 次。
//
// 模拟是可行的，因为两者要的是同一件事——让模型产出符合某个 JSON Schema
// 的结构化结果。structured outputs 由服务端约束解码，强制工具调用则由
// 工具入参的 schema 约束，二者的产出都是一份符合 schema 的 JSON。
// 实测同一个网关：原样请求 400，换成强制工具调用 200。
//
// 采用「先发原样、失效了再补救」而不是主动改写所有请求：
// 支持这个特性的上游（官方 API、部分中转）就该拿到真正的 structured outputs，
// 那是服务端保证的约束解码，比工具调用更强。
//
// 「失效」有两种形态，实测各出现在一个真实上游上：一类网关直接回 400，
// DeepSeek 则是既不报错也不遵守，返回一段散文——客户端 JSON.parse 照样炸。
// 只认 400 会漏掉后者，所以两种都要认。

// structuredToolName 是模拟用的工具名。
// 固定用一个名字，回译时才能准确认出该取哪个块。
const structuredToolName = "structured_output"

// toolNameSafe 是 Anthropic 对工具名的约束。
var toolNameSafe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// betaStructured 匹配要从降级请求里摘掉的 beta 特性。
// 降级之后已经不用这个特性了，还带着它既不诚实，
// 也可能撞上「未知 beta 值」这类另一种 400（实测网关会拒未知 beta）。
var betaStructured = regexp.MustCompile(`(?i)^structured-outputs-`)

// structuredPlan 是一次可降级请求的解析结果。
type structuredPlan struct {
	body   []byte // 原始请求体
	schema string // output_config.format.schema 的原始 JSON
	name   string // 工具名
}

// planStructuredFallback 判断这个请求能否用工具调用模拟，不能则返回 nil。
//
// 由持有请求体的那一层（proxy.ServeHTTP）调用一次，结论随 context 传下去。
// 放在传输层现算的话，每个请求都要复制一遍请求体——那可能是几 MB。
// bytes.Contains 那道预筛把绝大多数请求挡在 JSON 解析之外。
//
// 四个条件缺一不可：
//
//   - 带 output_config.format.schema——没有 schema 就无从构造工具；
//   - 不带 tools——请求里已有真工具时，强制调用本程序合成的工具会把那一轮
//     真正的工具使用顶掉。宁可如实返回 400，也不要悄悄改变语义。
//     实测 Claude Code 的结构化输出调用点全部 mcpTools:[]，不冲突；
//   - 非流式——同上，实测调用点走的都是 messages.create 不带 stream。
//     流式要另外合成一整套 SSE 事件，为一个不存在的形态写这些没有意义；
//   - POST /v1/messages——只有这个端点有 structured outputs。
func planStructuredFallback(path string, body []byte) *structuredPlan {
	if !strings.HasSuffix(path, "/v1/messages") ||
		!bytes.Contains(body, []byte(`"output_config"`)) {
		return nil
	}

	root := gjson.ParseBytes(body)
	format := root.Get("output_config.format")
	schema := format.Get("schema")
	if !schema.Exists() {
		return nil
	}
	if root.Get("tools").IsArray() && len(root.Get("tools").Array()) > 0 {
		return nil
	}
	if root.Get("stream").Bool() {
		return nil
	}

	name := structuredToolName
	if n := format.Get("name").String(); toolNameSafe.MatchString(n) {
		name = n
	}
	return &structuredPlan{body: body, schema: schema.Raw, name: name}
}

// buildStructuredRequest 把请求改写成一次强制工具调用。
func (p *structuredPlan) buildRequest() ([]byte, error) {
	out, err := sjson.DeleteBytes(p.body, "output_config")
	if err != nil {
		return nil, err
	}
	out, err = sjson.SetRawBytes(out, "tools", []byte(`[{"name":`+strconv.Quote(p.name)+
		`,"description":"Return the final result as structured data matching the schema.",`+
		`"input_schema":`+p.schema+`}]`))
	if err != nil {
		return nil, err
	}
	// 用 any 而不是 {"type":"tool","name":…}：工具只有一个，两者语义等价，
	// 但 any 的兼容面更宽——实测 DeepSeek 的思考模式直接拒绝后者
	// （400 Thinking mode does not support this tool_choice），any 则三个模型全通。
	return sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
}

// decodeStructuredResponse 把工具调用的结果翻回 structured outputs 的形态。
//
// Claude Code 的消费方式是把所有 text 块拼起来再 JSON.parse，
// 所以产出必须是一个装着 JSON 的 text 块，而不是 tool_use 块。
// stop_reason 也必须改回 end_turn：客户端见到 tool_use 会去找工具执行，
// 而这个「工具」根本不存在，会话就卡在那儿。
func decodeStructuredResponse(raw []byte, name string) ([]byte, bool) {
	root := gjson.ParseBytes(raw)
	var input string
	root.Get("content").ForEach(func(_, b gjson.Result) bool {
		if b.Get("type").String() == "tool_use" && b.Get("name").String() == name {
			input = b.Get("input").Raw
			return false
		}
		return true
	})
	if input == "" {
		return nil, false
	}
	out, err := sjson.SetRawBytes(raw, "content",
		[]byte(`[{"type":"text","text":`+strconv.Quote(input)+`}]`))
	if err != nil {
		return nil, false
	}
	out, err = sjson.SetBytes(out, "stop_reason", "end_turn")
	if err != nil {
		return nil, false
	}
	return out, true
}

// conformsToSchema 判断这次 200 响应是否真的是结构化产出。
//
// 光看状态码不够：实测 DeepSeek 对 output_config 既不报错也不遵守，
// 直接返回一段散文，客户端 JSON.parse 一样会炸——同一个功能坏在两处，
// 只是症状不同。判据取客户端自己的消费方式：把 text 块拼起来能否 JSON 解析。
// 用 thinking 块之外的 text 拼接，与 Claude Code 的 filter(type==="text") 一致。
func conformsToSchema(raw []byte, schemaRaw string) bool {
	var text strings.Builder
	gjson.GetBytes(raw, "content").ForEach(func(_, b gjson.Result) bool {
		if b.Get("type").String() == "text" {
			text.WriteString(b.Get("text").String())
		}
		return true
	})
	candidate := []byte(strings.TrimSpace(text.String()))
	if !json.Valid(candidate) || !json.Valid([]byte(schemaRaw)) {
		return false
	}
	compiler := jsonschema.NewCompiler()
	const schemaURL = "https://ccproxy.invalid/structured-schema.json"
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaRaw))
	if err != nil {
		return false
	}
	if err := compiler.AddResource(schemaURL, doc); err != nil {
		return false
	}
	sch, err := compiler.Compile(schemaURL)
	if err != nil {
		return false
	}
	var value any
	if err := json.Unmarshal(candidate, &value); err != nil {
		return false
	}
	return sch.Validate(value) == nil
}

// compileStructuredSchema is intentionally kept small and uses the mature
// validator dependency rather than a partial in-house JSON Schema subset.
func compileStructuredSchema(raw string) error {
	if !json.Valid([]byte(raw)) {
		return fmt.Errorf("invalid JSON schema")
	}
	c := jsonschema.NewCompiler()
	const u = "https://ccproxy.invalid/schema.json"
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return err
	}
	if err := c.AddResource(u, doc); err != nil {
		return err
	}
	_, err = c.Compile(u)
	return err
}

// structuredFallback 在结构化输出没能生效时补一次强制工具调用。
//
// 两种触发形态，实测各出现在一个真实上游上：
//
//   - 400：上游明确拒绝该特性。判据不看错误文案——
//     「带 schema 的请求收到 400」这个组合本身已经足够窄，
//     若这次 400 其实另有原因，补发会同样失败，届时原样返回最初那个 400，
//     客户端看到的和没有本函数时完全一致；
//   - 200 但内容不合规：上游静默忽略该特性（DeepSeek）。
//
// 返回 nil 表示不适用或补救失败，调用方应继续使用原响应。
func (t *retryTransport) structuredFallback(req *http.Request, resp *http.Response) *http.Response {
	plan, _ := req.Context().Value(ctxStructured).(*structuredPlan)
	if plan == nil {
		return nil
	}

	// 200 的情况要先看内容合不合规，合规就不该动它。
	// 此处能直接读明文，是因为 Rewrite 无条件剥掉了客户端的 Accept-Encoding：
	// Go 的 Transport 于是自己加 gzip，并在这一层之下透明解压。
	var passthru []byte
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		raw, over, err := readBodyPrefix(resp.Body, responseBodyCap)
		if err != nil || over {
			resp.Body = prependBody(raw, resp.Body)
			return nil
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && conformsToSchema(raw, plan.schema) {
			// 上游原生支持，原样放行——服务端约束解码比工具调用更强。
			resp.Body = io.NopCloser(bytes.NewReader(raw))
			return nil
		}
		passthru = raw
		if resp.StatusCode == http.StatusOK {
			t.logf.Printf("structured outputs fallback: 上游返回 200 但内容不是结构化产出，改用工具调用重试")
		}
	}

	newBody, err := plan.buildRequest()
	if err != nil {
		t.logf.Printf("structured outputs fallback: 改写请求失败: %v", err)
		return restoreBody(resp, passthru)
	}

	r := req.Clone(req.Context())
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newBody)), nil
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	stripStructuredBeta(r.Header)

	fb, err := t.roundTripRetries(r, false)
	if err != nil {
		t.logf.Printf("structured outputs fallback: 补发失败: %v", err)
		return restoreBody(resp, passthru)
	}
	if fb.StatusCode != http.StatusOK {
		// This physical fallback response never reaches the client. Drain the
		// complete body through the bounded usage tap before restoring the original.
		t.recordDiscardedBody(req, fb.Body, fb.ContentLength)
		t.logf.Printf("structured outputs fallback: 补发返回 %d，沿用原始响应", fb.StatusCode)
		return restoreBody(resp, passthru)
	}

	rawResp, over, err := readBodyPrefix(fb.Body, responseBodyCap)
	if err != nil || over {
		// Translation stays capped, but the discarded physical response must still
		// be fully drained and metered. Replay the consumed prefix exactly once.
		t.recordDiscardedBody(req, prependBody(rawResp, fb.Body), fb.ContentLength)
		t.logf.Printf("structured outputs fallback: 读取补发响应失败: %v", err)
		return restoreBody(resp, passthru)
	}
	_ = fb.Body.Close()
	decoded, ok := decodeStructuredResponse(rawResp, plan.name)
	if !ok {
		// The fallback body was fully consumed and discarded; account for its
		// real usage before restoring the original response for the client.
		t.recordDiscarded(req, rawResp)
		t.logf.Printf("structured outputs fallback: 补发响应里没有预期的工具调用，沿用原始响应")
		return restoreBody(resp, passthru)
	}

	// 原响应到这里才丢弃：走到这一步才确定不再需要它。
	// 无论是 200 不合规还是带 usage 的 400，都只在此处记录一次。
	_ = resp.Body.Close()
	// 丢弃的那一轮是真花过钱的：200 但内容不合规，意味着上游完整推理了一遍。
	// tap 挂在 ModifyResponse，只看得见最终返回的 fb，不在这里补记就永远漏账。
	// 无 usage 的错误体由 usageFromBody 解析为零，不会产生虚假计费。
	// Add 使用这里实际完成计量的时刻归日；不要强行归到请求开始日，跨午夜是合法的。
	t.recordDiscarded(req, passthru)

	fb.Body = io.NopCloser(bytes.NewReader(decoded))
	fb.ContentLength = int64(len(decoded))
	fb.Header.Set("Content-Type", "application/json")
	fb.Header.Set("Content-Length", strconv.Itoa(len(decoded)))
	fb.Header.Del("Content-Encoding")
	// ModifyResponse 靠 resp.Request 的 context 取路由目标，换了响应对象就得补上。
	fb.Request = req
	t.logf.Printf("structured outputs fallback: 已用强制工具调用模拟成功")
	return fb
}

// restoreBody 在补救失败时把读走的响应体还回去。
// 返回 nil 表示「用原响应」，这是调用方的约定。
func restoreBody(resp *http.Response, buf []byte) *http.Response {
	if buf != nil {
		resp.Body = io.NopCloser(bytes.NewReader(buf))
	}
	return nil
}

// stripStructuredBeta 摘掉 structured-outputs-* 特性声明，保留其余的。
func stripStructuredBeta(h http.Header) {
	cur := strings.TrimSpace(h.Get("Anthropic-Beta"))
	if cur == "" {
		return
	}
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(cur, ",") {
		if p := strings.TrimSpace(part); p != "" && !betaStructured.MatchString(p) {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		h.Del("Anthropic-Beta")
		return
	}
	h.Set("Anthropic-Beta", strings.Join(kept, ","))
}
