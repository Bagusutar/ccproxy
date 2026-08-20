package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const structSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`

// withPlan 把 ServeHTTP 那一步的产物塞进 context，模拟传输层看到的请求。
func withPlan(req *http.Request, plan *structuredPlan) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxStructured, plan))
}

func TestConformsToSchemaV6Keywords(t *testing.T) {
	cases := []struct {
		name, schema, value string
		want                bool
	}{
		{"type array", `{"type":"array","items":{"type":"string"}}`, `["a","b"]`, true},
		{"nullable", `{"type":["string","null"]}`, `null`, true},
		{"pattern", `{"type":"string","pattern":"^[A-Z]+$"}`, `"OK"`, true},
		{"minLength", `{"type":"string","minLength":3}`, `"ab"`, false},
		{"minimum", `{"type":"number","minimum":3}`, `2`, false},
		{"items", `{"type":"array","items":{"type":"integer"}}`, `[1,"x"]`, false},
		{"additionalProperties", `{"type":"object","properties":{"ok":{"type":"boolean"}},"additionalProperties":false}`, `{"ok":true,"extra":1}`, false},
		{"wrong shape", `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`, `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			textValue := strconv.Quote(tc.value)
			raw := []byte(`{"content":[{"type":"text","text":` + textValue + `}]}`)
			if got := conformsToSchema(raw, tc.schema); got != tc.want {
				t.Fatalf("conformsToSchema=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanStructuredFallbackAccepts(t *testing.T) {
	body := `{"model":"claude-opus-5","max_tokens":512,
	  "messages":[{"role":"user","content":"hi"}],
	  "output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`

	plan := planStructuredFallback("/v1/messages", []byte(body))
	if plan == nil {
		t.Fatal("应当识别为可降级请求")
	}
	if plan.name != structuredToolName {
		t.Fatalf("tool name = %q", plan.name)
	}

	out, err := plan.buildRequest()
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)
	if root.Get("output_config").Exists() {
		t.Error("output_config 应当被删除，否则上游还会以同样的理由拒绝")
	}
	if got := root.Get("tools.0.name").String(); got != structuredToolName {
		t.Errorf("tools.0.name = %q", got)
	}
	if got := root.Get("tools.0.input_schema.properties.ok.type").String(); got != "boolean" {
		t.Errorf("schema 未原样搬进 input_schema: %s", root.Get("tools.0.input_schema").Raw)
	}
	// any + 唯一一个工具 = 强制调用它，且比 {"type":"tool"} 兼容面更宽。
	if got := root.Get("tool_choice.type").String(); got != "any" {
		t.Errorf("tool_choice.type = %q，必须强制调用，否则模型可能直接回文本", got)
	}
	// 其余字段必须原样保留。
	if got := root.Get("max_tokens").Int(); got != 512 {
		t.Errorf("max_tokens = %d", got)
	}
}

func TestPlanStructuredFallbackRejects(t *testing.T) {
	cases := map[string]string{
		"没有 schema": `{"model":"m","messages":[]}`,
		"已带真工具": `{"model":"m","tools":[{"name":"Read","input_schema":{}}],
		  "output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`,
		"流式": `{"model":"m","stream":true,
		  "output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if planStructuredFallback("/v1/messages", []byte(body)) != nil {
				t.Fatal("不应降级")
			}
		})
	}

	t.Run("非 messages 端点", func(t *testing.T) {
		body := `{"output_config":{"format":{"schema":` + structSchema + `}}}`
		if planStructuredFallback("/v1/chat/completions", []byte(body)) != nil {
			t.Fatal("不应降级")
		}
	})

	t.Run("预筛不碰普通请求", func(t *testing.T) {
		if planStructuredFallback("/v1/messages", []byte(`{"model":"m","messages":[]}`)) != nil {
			t.Fatal("不应降级")
		}
	})
}

func TestDecodeStructuredResponse(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
	  "content":[{"type":"tool_use","id":"toolu_1","name":"structured_output",
	              "input":{"ok":true,"reason":"done"}}],
	  "stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`

	out, ok := decodeStructuredResponse([]byte(raw), structuredToolName)
	if !ok {
		t.Fatal("应当解码成功")
	}
	root := gjson.ParseBytes(out)
	// 客户端把所有 text 块拼起来再 JSON.parse，所以必须是 text 块。
	if got := root.Get("content.0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(root.Get("content.0.text").String()), &parsed); err != nil {
		t.Fatalf("text 不是合法 JSON: %v", err)
	}
	if parsed["ok"] != true {
		t.Errorf("解出的内容不对: %v", parsed)
	}
	// 留着 tool_use 的话客户端会去找一个根本不存在的工具执行。
	if got := root.Get("stop_reason").String(); got != "end_turn" {
		t.Errorf("stop_reason = %q，必须改回 end_turn", got)
	}
	if got := root.Get("usage.input_tokens").Int(); got != 10 {
		t.Errorf("usage 应当保留: %d", got)
	}
}

func TestDecodeStructuredResponseWithoutToolUse(t *testing.T) {
	raw := `{"content":[{"type":"text","text":"抱歉"}],"stop_reason":"end_turn"}`
	if _, ok := decodeStructuredResponse([]byte(raw), structuredToolName); ok {
		t.Fatal("没有工具调用时应当判定失败，让调用方沿用原始 400")
	}
}

func TestStripStructuredBeta(t *testing.T) {
	cases := []struct{ in, want string }{
		{"structured-outputs-2025-12-15", ""},
		{"context-1m-2025-08-07,structured-outputs-2025-12-15", "context-1m-2025-08-07"},
		{"context-1m-2025-08-07", "context-1m-2025-08-07"},
	}
	for _, c := range cases {
		h := http.Header{}
		h.Set("Anthropic-Beta", c.in)
		stripStructuredBeta(h)
		if got := h.Get("Anthropic-Beta"); got != c.want {
			t.Errorf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStructuredFallbackEndToEnd 用一个模仿此类网关的假上游跑通整条路径：
// 带 output_config 的请求一律 400，换成工具调用则正常返回。
func TestStructuredFallbackEndToEnd(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		root := gjson.ParseBytes(body)
		if root.Get("output_config").Exists() {
			seen = append(seen, "rejected")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w,
				`{"type":"error","error":{"message":"structured_outputs not supported in your workspace."}}`)
			return
		}
		seen = append(seen, "tool="+root.Get("tools.0.name").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"tool_use","id":"toolu_1","name":"structured_output",
		              "input":{"ok":true}}],"stop_reason":"tool_use"}`)
	}))
	defer srv.Close()

	body := `{"model":"claude-opus-5","max_tokens":512,
	  "messages":[{"role":"user","content":"目标达成了吗"}],
	  "output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req.Header.Set("Anthropic-Beta", "structured-outputs-2025-12-15")
	req = withPlan(req, planStructuredFallback("/v1/messages", []byte(body)))

	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0)}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端仍然看到 %d —— fallback 的全部意义就是不让 400 冒出去", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	root := gjson.ParseBytes(out)
	if got := root.Get("content.0.text").String(); !strings.Contains(got, `"ok"`) {
		t.Errorf("content.0.text = %q", got)
	}
	if got := root.Get("stop_reason").String(); got != "end_turn" {
		t.Errorf("stop_reason = %q", got)
	}
	want := []string{"rejected", "tool=structured_output"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("上游收到的请求序列 = %v, want %v", seen, want)
	}
	if resp.Request == nil {
		t.Error("resp.Request 必须补上，ModifyResponse 靠它的 context 取路由目标")
	}
}

func writeHugeFallback(w http.ResponseWriter, status int, prefix string) {
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(prefix))+responseBodyCap+1, 10))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, prefix)
	buf := bytes.Repeat([]byte{' '}, 32<<10)
	left := int64(responseBodyCap + 1)
	for left > 0 {
		n := min(int64(len(buf)), left)
		_, _ = w.Write(buf[:n])
		left -= n
	}
}

func structuredHugeFallbackFixture(t *testing.T, status int, prefix string) (*retryTransport, *http.Request, *http.Response, *usageMeter) {
	t.Helper()
	const original = `{"error":{"message":"unsupported"}}`
	body := `{"model":"m","messages":[],"output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req = withPlan(req, planStructuredFallback("/v1/messages", []byte(body)))
	req = req.WithContext(context.WithValue(req.Context(), ctxTarget, &target{id: "p", name: "provider", model: "m"}))
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(original)), Header: make(http.Header)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeHugeFallback(w, status, prefix) }))
	t.Cleanup(srv.Close)
	req.URL, _ = req.URL.Parse(srv.URL + "/v1/messages")
	meter := newUsageMeter()
	return &retryTransport{meter: meter, logf: quietLogger(), base: &http.Transport{}}, req, resp, meter
}

func TestStructuredFallbackMetersHugeNonOKFallback(t *testing.T) {
	tr, req, resp, meter := structuredHugeFallbackFixture(t, http.StatusUnprocessableEntity, `{"usage":{"input_tokens":31,"output_tokens":7}}`)
	if got := tr.structuredFallback(req, resp); got != nil {
		t.Fatal("failed fallback should restore the original response")
	}
	rows := meter.snapshot().Rows
	if len(rows) != 1 || rows[0].In != 31 || rows[0].Out != 7 {
		t.Fatalf("meter rows = %+v", rows)
	}
}

func TestStructuredFallbackMetersHugeOverCapSuccess(t *testing.T) {
	tr, req, resp, meter := structuredHugeFallbackFixture(t, http.StatusOK, `{"content":[{"type":"tool_use","name":"structured_output","input":{"ok":true}}],"usage":{"input_tokens":37,"output_tokens":11}}`)
	if got := tr.structuredFallback(req, resp); got != nil {
		t.Fatal("over-cap fallback should restore the original response")
	}
	rows := meter.snapshot().Rows
	if len(rows) != 1 || rows[0].In != 37 || rows[0].Out != 11 {
		t.Fatalf("meter rows = %+v", rows)
	}
}

func TestStructuredFallbackUsesRetryPolicyWithoutRecursiveFallback(t *testing.T) {
	var original, fallback int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "output_config").Exists() {
			original++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"unsupported"}}`)
			return
		}
		fallback++
		if fallback == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"tool_use","name":"structured_output","input":{"ok":true}}],"stop_reason":"tool_use"}`)
	}))
	defer srv.Close()
	old := retryBackoff
	retryBackoff = []time.Duration{0, 0, 0}
	defer func() { retryBackoff = old }()

	body := `{"model":"m","messages":[],"output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req = withPlan(req, planStructuredFallback("/v1/messages", []byte(body)))
	resp, err := (&retryTransport{base: &http.Transport{}, logf: quietLogger()}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if original != 1 || fallback != 2 {
		t.Fatalf("physical requests original=%d fallback=%d", original, fallback)
	}
	out, _ := io.ReadAll(resp.Body)
	if gjson.GetBytes(out, "stop_reason").String() != "end_turn" {
		t.Fatalf("fallback not decoded: %s", out)
	}
}

// 非 structured outputs 的 400 必须原样返回，且响应体完整可读——
// 补救路径全程不碰 resp.Body 就是为了保证这一点。
func TestPlainBadRequestUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"message":"max_tokens too large"}}`)
	}))
	defer srv.Close()

	body := `{"model":"m","max_tokens":9999999,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader([]byte(body)))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0)}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "max_tokens too large") {
		t.Errorf("原始错误体应当完整保留，实际 = %q", out)
	}
}

// 上游返回 200 却无视 schema（实测 DeepSeek 就是这样）时，
// 症状是客户端 JSON.parse 失败，不是 400——只认状态码会整个漏掉这一类。
func TestStructuredFallbackOnNonConforming(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if gjson.GetBytes(body, "output_config").Exists() {
			seen = append(seen, "ignored")
			// 既不报错也不遵守：thinking + 一段散文。
			_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant",
			  "content":[{"type":"thinking","thinking":"..."},
			             {"type":"text","text":"Yes — the goal was met."}],
			  "stop_reason":"end_turn"}`)
			return
		}
		seen = append(seen, "tool")
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant",
		  "content":[{"type":"tool_use","id":"t1","name":"structured_output",
		              "input":{"ok":true}}],"stop_reason":"tool_use"}`)
	}))
	defer srv.Close()

	body := `{"model":"deepseek-v4-flash","max_tokens":512,
	  "messages":[{"role":"user","content":"met?"}],
	  "output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req = withPlan(req, planStructuredFallback("/v1/messages", []byte(body)))

	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0)}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	text := gjson.GetBytes(out, "content.0.text").String()
	if !json.Valid([]byte(text)) {
		t.Fatalf("客户端拿到的仍然不是 JSON: %q", text)
	}
	if strings.Join(seen, ",") != "ignored,tool" {
		t.Errorf("上游收到的请求序列 = %v", seen)
	}
}

// 上游原生支持时不能多打一次请求，也不能改动响应。
func TestStructuredPassthroughWhenConforming(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	body := `{"model":"m","max_tokens":512,"messages":[],
	  "output_config":{"format":{"type":"json_schema","schema":` + structSchema + `}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	req = withPlan(req, planStructuredFallback("/v1/messages", []byte(body)))

	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0)}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if calls != 1 {
		t.Errorf("上游被调用 %d 次，原生支持时不该补发", calls)
	}
	if got := gjson.GetBytes(out, "content.0.text").String(); got != `{"ok":true}` {
		t.Errorf("响应被改动了: %q", got)
	}
}
