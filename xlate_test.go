package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAnthropicToResponsesRequest(t *testing.T) {
	in := `{
	  "model":"gpt-5.6-luna","max_tokens":100,"stream":true,
	  "system":[{"type":"text","text":"You are helpful."}],
	  "tools":[{"name":"read_file","description":"Read.","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"any"},
	  "messages":[
	    {"role":"user","content":"读文件"},
	    {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"/x"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"hello"}]}
	  ]}`
	out, err := anthropicToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(out)

	if g.Get("max_output_tokens").Int() != 100 {
		t.Errorf("max_output_tokens = %v", g.Get("max_output_tokens"))
	}
	if g.Get("instructions").String() != "You are helpful." {
		t.Errorf("instructions = %q", g.Get("instructions").String())
	}
	if !g.Get("stream").Bool() {
		t.Error("stream 丢了")
	}
	if g.Get("tools.0.type").String() != "function" || g.Get("tools.0.name").String() != "read_file" {
		t.Errorf("tools = %s", g.Get("tools").Raw)
	}
	// Anthropic 的 any 表示「必须用某个工具」，对应 Responses 的 required
	if g.Get("tool_choice").String() != "required" {
		t.Errorf("tool_choice = %v", g.Get("tool_choice"))
	}

	items := g.Get("input").Array()
	if len(items) != 3 {
		t.Fatalf("input 项数 = %d: %s", len(items), g.Get("input").Raw)
	}
	if items[0].Get("content.0.type").String() != "input_text" {
		t.Errorf("用户文本应是 input_text: %s", items[0].Raw)
	}
	if items[1].Get("type").String() != "function_call" || items[1].Get("call_id").String() != "call_1" {
		t.Errorf("tool_use 未翻成 function_call: %s", items[1].Raw)
	}
	if items[2].Get("type").String() != "function_call_output" ||
		items[2].Get("output").String() != "hello" {
		t.Errorf("tool_result 未翻成 function_call_output: %s", items[2].Raw)
	}
}

// max_tokens 太小时要抬到 16：推理型模型会先把额度花在思考上，
// 给 1 个 token 连一个完整响应对象都拿不到（实测 deepseek 返回 incomplete）。
func TestAnthropicToResponsesReplaysThinkingHistory(t *testing.T) {
	out, err := anthropicToResponses([]byte(`{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"},{"type":"text","text":"answer"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 1 || input[0].Get("content.0.text").String() != "answer" {
		t.Fatalf("translated thinking history = %s", out)
	}
}

func TestAnthropicToolResultImageToResponses(t *testing.T) {
	out, err := anthropicToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	output := gjson.GetBytes(out, "input.0.output")
	if !output.IsArray() || output.Get("0.type").String() != "input_text" || output.Get("0.text").String() != "screenshot" {
		t.Fatalf("text tool result not preserved: %s", out)
	}
	if output.Get("1.type").String() != "input_image" || output.Get("1.image_url").String() != "data:image/png;base64,YWJj" {
		t.Fatalf("image tool result not translated: %s", out)
	}
}

func TestAnthropicToResponsesPreservesToolResultErrorTextually(t *testing.T) {
	out, err := anthropicToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":true,"content":"boom"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	item := gjson.GetBytes(out, "input.0")
	if item.Get("output").String() != responsesToolErrorPrefix+"boom" || item.Get("is_error").Exists() {
		t.Fatalf("invalid error translation: %s", item.Raw)
	}
}

func TestAnthropicToResponsesNullToolChoiceIsOmitted(t *testing.T) {
	out, err := anthropicToResponses([]byte(`{"model":"m","messages":[],"tool_choice":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("null tool_choice should be omitted: %s", out)
	}
}

func TestAnthropicToResponsesToolChoiceAuto(t *testing.T) {
	out, err := anthropicToResponses([]byte(`{"model":"m","tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto", got)
	}
}

func TestNormalizeNullableObjectSchemaForResponses(t *testing.T) {
	raw, err := normalizeSchemaForOpenAI([]byte(`{"type":["object","null"]}`))
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(raw)
	if !g.Get("properties").IsObject() || !g.Get("required").IsArray() || g.Get("additionalProperties").Bool() {
		t.Fatalf("nullable object schema not normalized: %s", raw)
	}
}

func TestNormalizeEmptyObjectSchemaForResponses(t *testing.T) {
	raw, err := normalizeSchemaForOpenAI([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(raw)
	if !g.Get("properties").IsObject() || !g.Get("required").IsArray() || g.Get("additionalProperties").Bool() {
		t.Fatalf("empty object schema not normalized: %s", raw)
	}
}

func TestAnthropicToResponsesRaisesTinyMaxTokens(t *testing.T) {
	out, _ := anthropicToResponses([]byte(`{"model":"m","max_tokens":1,"messages":[]}`))
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 16 {
		t.Errorf("max_output_tokens = %d, want 16", got)
	}
}

func TestResponsesToAnthropic(t *testing.T) {
	in := `{"id":"resp_1","model":"gpt-5.6-luna","status":"completed",
	  "output":[
	    {"type":"reasoning","summary":[]},
	    {"type":"message","content":[{"type":"output_text","text":"好的"}]},
	    {"type":"function_call","call_id":"call_9","name":"read_file","arguments":"{\"path\":\"/etc/hosts\"}"}
	  ],
	  "usage":{"input_tokens":64,"input_tokens_details":{"cache_write_tokens":40,"cached_tokens":20},"output_tokens":21}}`
	out, err := responsesToAnthropic([]byte(in), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(out)

	if g.Get("type").String() != "message" || g.Get("role").String() != "assistant" {
		t.Errorf("外层形状不对: %s", out)
	}
	if g.Get("model").String() != "gpt-5.6-luna" {
		t.Errorf("model = %q", g.Get("model").String())
	}
	blocks := g.Get("content").Array()
	// reasoning 项刻意不透出：Anthropic 的 thinking 块要求带签名，伪造会被上游拒
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d: %s", len(blocks), g.Get("content").Raw)
	}
	if blocks[0].Get("type").String() != "text" || blocks[0].Get("text").String() != "好的" {
		t.Errorf("文本块不对: %s", blocks[0].Raw)
	}
	if blocks[1].Get("id").String() != "call_9" ||
		blocks[1].Get("input.path").String() != "/etc/hosts" {
		t.Errorf("工具块不对: %s", blocks[1].Raw)
	}
	if g.Get("stop_reason").String() != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", g.Get("stop_reason").String())
	}
	if g.Get("usage.input_tokens").Int() != 4 {
		t.Errorf("普通输入换算错误: %s", g.Get("usage").Raw)
	}
	if g.Get("usage.cache_creation_input_tokens").Int() != 40 ||
		g.Get("usage.cache_read_input_tokens").Int() != 20 ||
		g.Get("usage.output_tokens").Int() != 21 {
		t.Errorf("Anthropic 四字段 usage 不完整: %s", g.Get("usage").Raw)
	}
}

// stop_reason 映射错了整轮对话就废了：Claude Code 收到 end_turn 之外的值
// 会分别判成 truncated / refused / unexpected_stop。
func TestResponsesFailedNonstreamReturnsError(t *testing.T) {
	body := []byte(`{"status":"failed","error":{"message":"boom"},"output":[]}`)
	if _, err := responsesToAnthropic(body, "m"); err == nil {
		t.Error("failed Responses object translated as successful Anthropic message")
	}
	if _, err := responsesToChat(body, "m"); err == nil {
		t.Error("failed Responses object translated as successful Chat completion")
	}
}

func TestResponsesRefusalNonstreamPreserved(t *testing.T) {
	body := []byte(`{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"cannot comply"}]}]}`)
	a, err := responsesToAnthropic(body, "m")
	if err != nil || gjson.GetBytes(a, "content.0.text").String() != "cannot comply" || gjson.GetBytes(a, "stop_reason").String() != "refusal" {
		t.Fatalf("Anthropic refusal lost: %s err=%v", a, err)
	}
	c, err := responsesToChat(body, "m")
	if err != nil || gjson.GetBytes(c, "choices.0.message.refusal").String() != "cannot comply" || gjson.GetBytes(c, "choices.0.finish_reason").String() != "content_filter" {
		t.Fatalf("Chat refusal lost: %s err=%v", c, err)
	}
}

func TestResponsesStopReason(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"status":"completed","output":[{"type":"message","content":[]}]}`, "end_turn"},
		{`{"status":"incomplete","output":[{"type":"message","content":[]}]}`, "max_tokens"},
		{`{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"n","arguments":"{}"}]}`, "tool_use"},
	}
	for _, c := range cases {
		out, _ := responsesToAnthropic([]byte(c.body), "m")
		if got := gjson.GetBytes(out, "stop_reason").String(); got != c.want {
			t.Errorf("%s → stop_reason=%q, want %q", c.body, got, c.want)
		}
	}
}

// 流式翻译必须产出完整合法的 Anthropic 事件序列，且是增量的。
func TestSSEXlateBoundedLinesAndRecovery(t *testing.T) {
	const cap = sseEventCap
	for _, tc := range []struct {
		name, line string
	}{
		{"event", "event: "},
		{"comment", ":"},
		{"unknown", "unknown: "},
		{"data", "data: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			b.WriteString("event: response.created\n")
			b.WriteString("data: {\"response\":{\"id\":\"discard\"}}\n")
			b.WriteString(tc.line)
			b.Write(bytes.Repeat([]byte{'x'}, cap+1024))
			b.WriteString("\n\n")
			b.WriteString("event: response.completed\r\n")
			b.WriteString("data: {\"response\":{\"status\":\"completed\"}}\r\n\r\n")
			var events []string
			s := newSSEXlate(io.NopCloser(bytes.NewReader(b.Bytes())))
			s.handle = func(event string, data []byte) { events = append(events, event+":"+string(data)) }
			if _, err := io.ReadAll(&s); err != nil {
				t.Fatal(err)
			}
			// An oversized field invalidates the entire current SSE record;
			// only the following bounded record may be dispatched.
			if len(events) != 1 || events[0] != `response.completed:{"response":{"status":"completed"}}` {
				t.Fatalf("events = %q", events)
			}
		})
	}
}

func TestSSEXlateEventSemantics(t *testing.T) {
	cases := []struct {
		name, input, wantEvent, wantData string
		wantCalls                        int
	}{
		{"event-only-blank", "event: ignored\n\n", "", "", 0},
		{"event-only-eof", "event: ignored", "", "", 0},
		{"empty-data", "event: empty\ndata:\n\n", "empty", "", 1},
		{"event-spaces", "event:  lead  \ndata: x\n\n", " lead  ", "x", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSSEXlate(io.NopCloser(strings.NewReader(tc.input)))
			calls := 0
			var gotEvent, gotData string
			s.handle = func(event string, data []byte) { calls++; gotEvent, gotData = event, string(data) }
			_, _ = io.ReadAll(&s)
			if calls != tc.wantCalls || gotEvent != tc.wantEvent || gotData != tc.wantData {
				t.Fatalf("got calls=%d event=%q data=%q", calls, gotEvent, gotData)
			}
		})
	}
}

func TestSSEXlateOversizedFieldsRecoverWithoutPoisoning(t *testing.T) {
	for _, field := range []string{"event", "data", ":", "unknown"} {
		t.Run(field, func(t *testing.T) {
			input := "event: ok\ndata: good\n" + field + ":" + strings.Repeat("x", sseEventCap+1024) + "\n\n" +
				"event: recovered\r\ndata: yes\r\n\r\n"
			s := newSSEXlate(io.NopCloser(strings.NewReader(input)))
			var got []string
			s.handle = func(event string, data []byte) { got = append(got, event+":"+string(data)) }
			_, _ = io.ReadAll(&s)
			if strings.Join(got, ",") != "recovered:yes" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestSSEXlateMultiDataAndEOF(t *testing.T) {
	input := "event: response.completed\n" +
		"data: {\"a\":\n" + "data: 1}\n"
	s := newSSEXlate(io.NopCloser(strings.NewReader(input)))
	var got string
	s.handle = func(event string, data []byte) { got = event + ":" + string(data) }
	if _, err := io.ReadAll(&s); err != nil {
		t.Fatal(err)
	}
	if got != "response.completed:{\"a\":\n1}" {
		t.Fatalf("got %q", got)
	}
}

func TestResponsesStreamTranslation(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_7","name":"read_file"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"{\"path\""}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":":\"/etc/hosts\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`,
		``,
	}, "\n")

	s := newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "gpt-5.6-luna", false)
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	var argJSON strings.Builder
	var stop string
	for _, line := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			d := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if pj := d.Get("delta.partial_json"); pj.Exists() {
				argJSON.WriteString(pj.String())
			}
			if sr := d.Get("delta.stop_reason"); sr.Exists() {
				stop = sr.String()
			}
		}
	}

	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列 = %v\nwant %v", names, want)
	}
	// reasoning 项不该产生任何块——它出现在上游流里但必须被吞掉
	if strings.Count(strings.Join(names, ","), "content_block_start") != 1 {
		t.Error("reasoning 项不该开出一个块")
	}
	// 拼起来必须是合法 JSON：客户端就是这么还原工具入参的
	var parsed map[string]any
	if err := json.Unmarshal([]byte(argJSON.String()), &parsed); err != nil {
		t.Fatalf("拼接后的入参不是合法 JSON: %q", argJSON.String())
	}
	if parsed["path"] != "/etc/hosts" {
		t.Errorf("入参 = %v", parsed)
	}
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
}

// 上游中途报错时也要闭合已开的块，否则客户端会一直等一个永远不来的
// content_block_stop，表现为界面卡在半截输出上。
func TestResponsesStreamClosesBlockOnError(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"response":{"id":"r"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"item":{"type":"message"}}`,
		``,
		// 必须已经吐过内容，块才真的开着——文本块是延迟到第一段增量才开的。
		`event: response.output_text.delta`,
		`data: {"delta":"半"}`,
		``,
		`event: error`,
		`data: {"message":"boom"}`,
		``,
	}, "\n")
	s := newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", false)
	got, _ := io.ReadAll(s)
	if !strings.Contains(string(got), "content_block_stop") {
		t.Errorf("报错时没有闭合块:\n%s", got)
	}
	if !strings.Contains(string(got), "event: error") {
		t.Errorf("没有把错误透出给客户端:\n%s", got)
	}
}

// 路由决策：原生命中就透传，只有原生不通且我们会翻时才翻译。
func TestPickUpstreamProtocol(t *testing.T) {
	p := &Provider{ModelProtocols: map[string][]string{
		"claude-opus-5": {"anthropic", "chat"},
		"gpt-5.6-luna":  {"responses"},
		"chat-only":     {"chat"},
	}}
	cases := []struct {
		model     string
		client    Protocol
		wantProto Protocol
		wantXlate bool
	}{
		{"claude-opus-5", ProtoAnthropic, ProtoAnthropic, false}, // 原生，透传
		{"claude-opus-5", ProtoChat, ProtoChat, false},           // 原生，透传
		{"gpt-5.6-luna", ProtoAnthropic, ProtoResponses, true},   // 翻译
		{"gpt-5.6-luna", ProtoResponses, ProtoResponses, false},  // Codex 直连，透传
		// 只有 chat 的模型：Anthropic 客户端要它，但 chat 方向还没实现，
		// 于是原样发出去让上游如实报错，而不是擅自改写。
		{"chat-only", ProtoAnthropic, ProtoAnthropic, false},
		// 没测过的模型一律按原生处理：没有依据就不该改写请求。
		{"never-probed", ProtoAnthropic, ProtoAnthropic, false},
	}
	for _, c := range cases {
		proto, xl := pickUpstreamProtocol(p, c.model, c.client)
		if proto != c.wantProto || xl != c.wantXlate {
			t.Errorf("%s/%s → (%s, %v), want (%s, %v)",
				c.model, c.client, proto, xl, c.wantProto, c.wantXlate)
		}
	}
}

func TestClientProtocol(t *testing.T) {
	cases := map[string]Protocol{
		"/v1/messages":              ProtoAnthropic,
		"/v1/messages/count_tokens": ProtoAnthropic,
		"/v1/chat/completions":      ProtoChat,
		"/v1/responses":             ProtoResponses,
		"/foo/bar":                  ProtoAnthropic,
	}
	for path, want := range cases {
		if got := clientProtocol(path); got != want {
			t.Errorf("clientProtocol(%q) = %s, want %s", path, got, want)
		}
	}
}

// 双地址填全时，翻译目标是 Responses 就该用 OpenAI 地址——
// 判据是「用哪种方言发出去」，不是「客户端从哪个路径进来」。
func TestBaseForProtocol(t *testing.T) {
	dual := &Provider{BaseURL: "https://api.deepseek.com/anthropic",
		OpenAIBaseURL: "https://api.deepseek.com"}
	if got := dual.BaseForProtocol(ProtoAnthropic); got != "https://api.deepseek.com/anthropic" {
		t.Errorf("anthropic → %q", got)
	}
	if got := dual.BaseForProtocol(ProtoResponses); got != "https://api.deepseek.com" {
		t.Errorf("responses → %q", got)
	}
	single := &Provider{BaseURL: "https://gw.example.com"}
	for _, p := range protocolOrder {
		if got := single.BaseForProtocol(p); got != "https://gw.example.com" {
			t.Errorf("单地址 %s → %q", p, got)
		}
	}
}

// ---------- chat/completions <-> Responses（Cursor 那条路）----------

func TestChatToResponsesNullToolChoiceIsOmitted(t *testing.T) {
	out, err := chatToResponses([]byte(`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("null tool_choice should be omitted: %s", out)
	}
}

func TestChatToResponsesRequest(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","max_completion_tokens":200,"stream":true,
	  "messages":[
	    {"role":"system","content":"You are helpful."},
	    {"role":"user","content":"读文件"},
	    {"role":"assistant","tool_calls":[{"id":"call_1","type":"function",
	      "function":{"name":"read_file","arguments":"{\"path\":\"/x\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"hello"}],
	  "tools":[{"type":"function","function":{"name":"read_file",
	    "description":"Read.","parameters":{"type":"object"}}}],
	  "tool_choice":"required"}`
	out, err := chatToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(out)

	if g.Get("instructions").String() != "You are helpful." {
		t.Errorf("system 未提到 instructions: %q", g.Get("instructions").String())
	}
	if g.Get("max_output_tokens").Int() != 200 {
		t.Errorf("max_completion_tokens 未映射: %v", g.Get("max_output_tokens"))
	}
	items := g.Get("input").Array()
	if len(items) != 3 { // system 提走了，剩 user / function_call / function_call_output
		t.Fatalf("input 项数 = %d: %s", len(items), g.Get("input").Raw)
	}
	if items[1].Get("type").String() != "function_call" ||
		items[1].Get("call_id").String() != "call_1" {
		t.Errorf("tool_calls 未翻成 function_call: %s", items[1].Raw)
	}
	if items[2].Get("type").String() != "function_call_output" ||
		items[2].Get("output").String() != "hello" {
		t.Errorf("tool 角色未翻成 function_call_output: %s", items[2].Raw)
	}
	if g.Get("tools.0.name").String() != "read_file" {
		t.Errorf("tools 未展平: %s", g.Get("tools").Raw)
	}
	if g.Get("tool_choice").String() != "required" {
		t.Errorf("tool_choice = %v", g.Get("tool_choice"))
	}
}

// 旧字段 max_tokens 也要认：不同客户端用的不一样。
func TestChatToResponsesLegacyMaxTokens(t *testing.T) {
	out, _ := chatToResponses([]byte(`{"model":"m","max_tokens":77,"messages":[]}`))
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 77 {
		t.Errorf("max_output_tokens = %d, want 77", got)
	}
}

func TestResponsesToChat(t *testing.T) {
	in := `{"id":"resp_1","model":"gpt-5.6-luna","status":"completed","created_at":123,
	  "output":[{"type":"reasoning"},
	    {"type":"message","content":[{"type":"output_text","text":"好的"}]},
	    {"type":"function_call","call_id":"call_9","name":"read_file","arguments":"{}"}],
	  "usage":{"input_tokens":64,"input_tokens_details":{"cache_write_tokens":40,"cached_tokens":20},"output_tokens":21}}`
	out, err := responsesToChat([]byte(in), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(out)
	if g.Get("object").String() != "chat.completion" {
		t.Errorf("object = %q", g.Get("object").String())
	}
	if g.Get("choices.0.message.content").String() != "好的" {
		t.Errorf("content = %q", g.Get("choices.0.message.content").String())
	}
	if g.Get("choices.0.message.tool_calls.0.id").String() != "call_9" {
		t.Errorf("tool_calls 不对: %s", g.Get("choices.0.message.tool_calls").Raw)
	}
	if g.Get("choices.0.finish_reason").String() != "tool_calls" {
		t.Errorf("finish_reason = %q", g.Get("choices.0.finish_reason").String())
	}
	if g.Get("usage.total_tokens").Int() != 85 {
		t.Errorf("total_tokens = %v", g.Get("usage.total_tokens"))
	}
	if g.Get("usage.prompt_tokens").Int() != 64 ||
		g.Get("usage.prompt_tokens_details.cache_write_tokens").Int() != 40 ||
		g.Get("usage.prompt_tokens_details.cached_tokens").Int() != 20 {
		t.Errorf("Chat 缓存 usage 不完整: %s", g.Get("usage").Raw)
	}
}

// 没有文本时 content 必须是 null 而不是缺字段——Cursor 会直接取这个字段。
func TestResponsesToChatNullContent(t *testing.T) {
	out, _ := responsesToChat([]byte(`{"status":"completed","output":[
	  {"type":"function_call","call_id":"c","name":"n","arguments":"{}"}]}`), "m")
	if !gjson.GetBytes(out, "choices.0.message.content").Exists() {
		t.Errorf("content 字段缺失: %s", out)
	}
}

func TestResponsesChatStreamTranslation(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"response":{"id":"resp_1","created_at":9}}`, ``,
		`event: response.output_item.added`,
		`data: {"item":{"type":"reasoning"}}`, ``,
		`event: response.output_item.added`,
		`data: {"item":{"type":"message"}}`, ``,
		`event: response.output_text.delta`,
		`data: {"delta":"你好"}`, ``,
		`event: response.output_item.added`,
		`data: {"item":{"type":"function_call","call_id":"call_7","name":"read_file"}}`, ``,
		`event: response.function_call_arguments.delta`,
		`data: {"delta":"{\"path\""}`, ``,
		`event: response.function_call_arguments.delta`,
		`data: {"delta":":\"/x\"}"}`, ``,
		`event: response.completed`,
		`data: {"response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`, ``,
	}, "\n")

	s := newResponsesChatStream(io.NopCloser(strings.NewReader(upstream)), "gpt-5.6-luna", false)
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)

	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("缺少 [DONE] 终止哨兵，客户端会一直等:\n%s", body)
	}
	var text, args strings.Builder
	var finish string
	var toolID, toolName string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		d := gjson.Parse(strings.TrimPrefix(line, "data: "))
		if d.Get("object").String() != "chat.completion.chunk" {
			t.Errorf("chunk 的 object 不对: %s", line)
		}
		ch := d.Get("choices.0")
		text.WriteString(ch.Get("delta.content").String())
		ch.Get("delta.tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if id := tc.Get("id").String(); id != "" {
				toolID, toolName = id, tc.Get("function.name").String()
			}
			args.WriteString(tc.Get("function.arguments").String())
			return true
		})
		if fr := ch.Get("finish_reason"); fr.Type == gjson.String {
			finish = fr.String()
		}
	}
	if text.String() != "你好" {
		t.Errorf("文本 = %q", text.String())
	}
	// id 与 name 只该出现一次（首个 chunk），后续只带 arguments 增量
	if toolID != "call_7" || toolName != "read_file" {
		t.Errorf("工具标识 = %q / %q", toolID, toolName)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("拼接后的入参不是合法 JSON: %q", args.String())
	}
	if parsed["path"] != "/x" {
		t.Errorf("入参 = %v", parsed)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
}

// 一律不让上游按客户端的偏好压缩：Go 只在自己加 Accept-Encoding 时才透明
// 解压，客户端自带 br/zstd 的话，代理任何想读响应体的地方都会静默失灵——
// 翻译解出空回复、structured outputs 判据失效、错误体截断、403 文案改写不生效，
// 四条路径都栽在同一件事上。
func TestRewriteAlwaysStripsAcceptEncoding(t *testing.T) {
	p := NewProxy(DefaultConfig(), log.New(io.Discard, "", 0))
	u, _ := url.Parse("https://upstream.test")

	for _, name := range []string{"翻译路径", "透传路径"} {
		xlate := ProtoResponses
		if name == "透传路径" {
			xlate = ""
		}
		t.Run(name, func(t *testing.T) {
			in := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			in.Header.Set("Accept-Encoding", "gzip, br, zstd")
			in = in.WithContext(context.WithValue(in.Context(), ctxTarget,
				&target{url: u, token: "tok", xlate: xlate}))

			pr := &httputil.ProxyRequest{In: in, Out: in.Clone(in.Context())}
			p.rp.Rewrite(pr)

			if got := pr.Out.Header.Get("Accept-Encoding"); got != "" {
				t.Fatalf("Accept-Encoding = %q，应当被删掉", got)
			}
		})
	}
}

func TestAnthropicToResponsesMapsOutputConfig(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","max_tokens":512,
	  "messages":[{"role":"user","content":"met?"}],
	  "output_config":{"format":{"type":"json_schema",
	    "schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}}}`
	out, err := anthropicToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)
	if got := root.Get("text.format.type").String(); got != "json_schema" {
		t.Fatalf("text.format.type = %q", got)
	}
	if got := root.Get("text.format.name").String(); got == "" {
		t.Error("name 是 Responses 侧的必填项，必须兜一个默认值")
	}
	if got := root.Get("text.format.schema.properties.ok.type").String(); got != "boolean" {
		t.Errorf("schema 未原样搬过去: %s", root.Get("text.format.schema").Raw)
	}
	// Responses 一律要求 required 覆盖全部属性（与 strict 无关），
	// 缺席的键要补进 required 并允许 null。
	if got := root.Get("text.format.schema.required").String(); got != `["ok"]` {
		t.Errorf("required = %s", got)
	}
}

func TestNormalizeSchemaForOpenAI(t *testing.T) {
	in := `{"type":"object",
	  "properties":{"ok":{"type":"boolean"},"reason":{"type":"string"},
	                "items":{"type":"array","items":{"type":"object",
	                  "properties":{"a":{"type":"string"}}}}},
	  "required":["ok"]}`
	out, err := normalizeSchemaForOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)
	if got := root.Get("required").String(); got != `["items","ok","reason"]` {
		t.Errorf("required 必须覆盖全部属性: %s", got)
	}
	if !root.Get("additionalProperties").Exists() || root.Get("additionalProperties").Bool() {
		t.Error("additionalProperties 必须显式为 false")
	}
	// 原本必填的键不该被改动。
	if got := root.Get("properties.ok.type").String(); got != "boolean" {
		t.Errorf("properties.ok = %s", root.Get("properties.ok").Raw)
	}
	// 原本选填的键改成「必须出现、可以是 null」。
	if got := root.Get("properties.reason.anyOf.1.type").String(); got != "null" {
		t.Errorf("properties.reason = %s", root.Get("properties.reason").Raw)
	}
	// 嵌套对象同样要处理，否则上游只会在更深一层报同一个错。
	nested := root.Get("properties.items.anyOf.0.items")
	if got := nested.Get("required").String(); got != `["a"]` {
		t.Errorf("嵌套对象未归一化: %s", nested.Raw)
	}
	if nested.Get("additionalProperties").Bool() {
		t.Errorf("嵌套对象缺 additionalProperties:false: %s", nested.Raw)
	}
}

func TestNullableSchemaIdempotent(t *testing.T) {
	in := `{"type":"object","properties":{"title":{"anyOf":[{"type":"string"},{"type":"null"}]}},
	  "required":[]}`
	out, _ := normalizeSchemaForOpenAI([]byte(in))
	// 已经允许 null 的不该再包一层，否则嵌套会越来越深。
	if got := gjson.GetBytes(out, "properties.title.anyOf.0.anyOf").Exists(); got {
		t.Errorf("重复包裹: %s", gjson.GetBytes(out, "properties.title").Raw)
	}
}

func TestChatToResponsesMapsJSONObjectResponseFormat(t *testing.T) {
	out, err := chatToResponses([]byte(`{"model":"m","messages":[],"response_format":{"type":"json_object"}}`))
	if err != nil || gjson.GetBytes(out, "text.format.type").String() != "json_object" || gjson.GetBytes(out, "text.format.schema").Exists() {
		t.Fatalf("json_object mapping = %s err=%v", out, err)
	}
}

func TestChatToResponsesMapsResponseFormat(t *testing.T) {
	in := `{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"met?"}],
	  "response_format":{"type":"json_schema","json_schema":{"name":"verdict",
	    "schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}}`
	out, err := chatToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)
	if got := root.Get("text.format.name").String(); got != "verdict" {
		t.Fatalf("text.format.name = %q，客户端给了名字就该用它", got)
	}
	// 该 fixture 的 required 是空的，所以 ok 会被归一化成「必须出现、可以是 null」。
	if got := root.Get("text.format.schema.properties.ok.anyOf.0.type").String(); got != "boolean" {
		t.Errorf("schema 丢了: %s", root.Get("text.format").Raw)
	}
	if got := root.Get("text.format.schema.required").String(); got != `["ok"]` {
		t.Errorf("required = %s", got)
	}
}

// OpenAI 客户端普遍用「内容分片数组」而不是纯字符串——带图片时必须用。
// 直接取 .String() 会把整个数组当成 JSON 文本塞给模型，图片则彻底丢失。
func TestChatToResponsesArrayContent(t *testing.T) {
	in := `{"model":"m","messages":[
	  {"role":"system","content":[{"type":"text","text":"be terse"}]},
	  {"role":"user","content":[{"type":"text","text":"看这张图"},
	     {"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},
	  {"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"结果"}]}]}`
	out, err := chatToResponses([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	root := gjson.ParseBytes(out)

	if got := root.Get("instructions").String(); got != "be terse" {
		t.Errorf("instructions = %q，分片数组没被拼成文本", got)
	}
	c := root.Get("input.0.content")
	if got := c.Get("0.text").String(); got != "看这张图" {
		t.Errorf("文本分片 = %q", got)
	}
	if got := c.Get("0.type").String(); got != "input_text" {
		t.Errorf("文本分片类型 = %q", got)
	}
	if got := c.Get("1.type").String(); got != "input_image" {
		t.Errorf("图片分片丢了: %s", c.Raw)
	}
	// Responses 的 input_image.image_url 是裸字符串，不是对象。
	if got := c.Get("1.image_url").String(); got != "data:image/png;base64,AAA" {
		t.Errorf("image_url = %q", got)
	}
	if got := root.Get("input.1.output").String(); got != "结果" {
		t.Errorf("tool 结果 = %q", got)
	}
}

// 助手轮的文本要标成 output_text，否则上游会把它当成新的用户输入。
func TestChatToResponsesAssistantArrayContent(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"assistant","content":[{"type":"text","text":"好的"}]}]}`
	out, _ := chatToResponses([]byte(in))
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "output_text" {
		t.Errorf("type = %q", got)
	}
}

// 上游开了消息项却一个字都没产出时，不能留下空文本块：那个块会一直躺在
// 客户端历史里，之后每一轮回传都换来
// 400 messages: text content blocks must be non-empty。
func TestResponsesStreamNoEmptyTextBlock(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"response":{"id":"r"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"item":{"type":"message"}}`,
		``,
		`event: response.output_item.done`,
		`data: {"item":{"type":"message"}}`,
		``,
		`event: response.completed`,
		`data: {"response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":0}}}`,
		``,
	}, "\n")
	s := newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", false)
	got, _ := io.ReadAll(s)
	if strings.Contains(string(got), "content_block_start") {
		t.Errorf("没有任何文本却开了块:\n%s", got)
	}
	if !strings.Contains(string(got), "message_stop") {
		t.Errorf("流没有正常收尾:\n%s", got)
	}
}

func TestResponsesToAnthropicSkipsEmptyText(t *testing.T) {
	in := `{"id":"r","status":"completed","output":[
	  {"type":"message","content":[{"type":"output_text","text":""}]}],
	  "usage":{"input_tokens":5,"output_tokens":0}}`
	out, err := responsesToAnthropic([]byte(in), "m")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(gjson.GetBytes(out, "content").Array()); n != 0 {
		t.Errorf("空文本块不该产出: %s", gjson.GetBytes(out, "content").Raw)
	}
}

// 客户端标为可选的属性，翻译时会被改成「必填 + 可为 null」以满足 Responses；
// 回程必须把 null 摘掉，否则客户端按自己那份原始 schema 校验会失败。
// 实测现象：Claude Code 的 /goal 停止钩子报
// "expected boolean, received null" at path ["impossible"]。
func TestStripsNullsFromStructuredOutput(t *testing.T) {
	in := []byte(`{"type":"message","role":"assistant","content":[
	  {"type":"text","text":"{\"ok\":true,\"reason\":\"done\",\"impossible\":null,` +
		`\"nested\":{\"a\":1,\"b\":null},\"list\":[{\"c\":null,\"d\":2}]}"}],
	  "stop_reason":"end_turn"}`)

	shape := structuredNullShapeFromSchema(`{"type":"object","properties":{"ok":{"type":"boolean"},"reason":{"type":"string"},"impossible":{"type":"boolean"},"nested":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a"]},"list":{"type":"array","items":{"type":"object","properties":{"c":{"type":"integer"},"d":{"type":"integer"}},"required":["d"]}}},"required":["ok","reason"]}`)
	out := stripNullFieldsWithShape(in, shape)
	txt := gjson.GetBytes(out, "content.0.text").String()
	var got map[string]any
	if err := json.Unmarshal([]byte(txt), &got); err != nil {
		t.Fatalf("产出不再是合法 JSON: %v\n%s", err, txt)
	}
	if _, ok := got["impossible"]; ok {
		t.Error("顶层的 null 没摘掉——客户端会报 expected boolean, received null")
	}
	if got["ok"] != true || got["reason"] != "done" {
		t.Errorf("非 null 字段被动了: %v", got)
	}
	if n, _ := got["nested"].(map[string]any); n == nil {
		t.Error("nested 丢了")
	} else if _, ok := n["b"]; ok {
		t.Error("嵌套对象里的 null 没摘掉")
	} else if n["a"] != float64(1) {
		t.Errorf("nested.a 被动了: %v", n["a"])
	}
	if l, _ := got["list"].([]any); len(l) != 1 {
		t.Errorf("数组丢了: %v", got["list"])
	} else if e, _ := l[0].(map[string]any); e == nil {
		t.Error("数组元素丢了")
	} else if _, ok := e["c"]; ok {
		t.Error("数组元素里的 null 没摘掉")
	}
}

// 没有 null 时必须原样返回同一块内存——绝大多数结构化产出都不含 null，
// 不该为它们白白重建一遍响应体。
func TestStripNullsNoopWhenClean(t *testing.T) {
	in := []byte(`{"content":[{"type":"text","text":"{\"ok\":true}"}]}`)
	shape := structuredNullShapeFromSchema(`{"type":"object","properties":{"ok":{"type":"boolean"},"reason":{"type":"string"},"impossible":{"type":"boolean"},"nested":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a"]},"list":{"type":"array","items":{"type":"object","properties":{"c":{"type":"integer"},"d":{"type":"integer"}},"required":["d"]}}},"required":["ok","reason"]}`)
	out := stripNullFieldsWithShape(in, shape)
	if &out[0] != &in[0] {
		t.Error("干净产出被重建了")
	}
	// 非 JSON 的普通文本也不能动
	plain := []byte(`{"content":[{"type":"text","text":"这不是 JSON"}]}`)
	if o := stripNullFieldsWithShape(plain, shape); &o[0] != &plain[0] {
		t.Error("普通文本被动了")
	}
}

// ---------- 流式路径的 null 清理 ----------

// sseTexts 把一段 Anthropic SSE 里的 text_delta 拼起来，并返回事件名序列。
func sseTexts(raw string) (text string, names []string) {
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			d := gjson.Parse(strings.TrimPrefix(line, "data: "))
			if t := d.Get("delta.text"); t.Exists() {
				b.WriteString(t.String())
			}
		}
	}
	return b.String(), names
}

// structuredUpstream 造一段把 JSON 逐字切碎的 Responses 流。
// 切碎是关键：真实上游就是这么发的，而 null 往往横跨好几个分片。
func structuredUpstream(payload string) string {
	lines := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"message"}}`,
		``,
	}
	for _, chunk := range strings.Split(payload, "") {
		q, _ := json.Marshal(chunk)
		lines = append(lines,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":`+string(q)+`}`,
			``)
	}
	return strings.Join(append(lines,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":9,"output_tokens":4}}}`,
		``,
	), "\n")
}

// 这条守的是 /goal 那个真实故障：normalizeSchemaForOpenAI 把可选属性改成
// 「必填 + 可为 null」，模型填了 null，而客户端按原始 schema 校验会拒。
// 非流式早就清了，流式当时整条分支都没接上——同一个请求加个 stream:true
// 就能复现出 {"achieved":false,"impossible":null}。
func TestStreamStripsNullsFromStructuredOutput(t *testing.T) {
	upstream := structuredUpstream(`{"achieved":false,"blockedBy":null,"impossible":null}`)
	shape := structuredNullShapeFromSchema(`{"type":"object","properties":{"achieved":{"type":"boolean"},"blockedBy":{"type":"string"},"impossible":{"type":"boolean"}},"required":["achieved"]}`)
	s := newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", true, shape)
	raw, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	text, names := sseTexts(string(raw))

	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("产出不是合法 JSON: %q", text)
	}
	if _, ok := got["impossible"]; ok {
		t.Errorf("null 字段没摘掉: %q", text)
	}
	if _, ok := got["blockedBy"]; ok {
		t.Errorf("null 字段没摘掉: %q", text)
	}
	if got["achieved"] != false {
		t.Errorf("非 null 的值被改动了: %v", got["achieved"])
	}
	// 攒齐再发，所以只应有一个 delta；块的开合仍要成对。
	if n := strings.Count(strings.Join(names, ","), "content_block_delta"); n != 1 {
		t.Errorf("content_block_delta = %d 个，攒齐后应只发 1 个", n)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列 = %v\nwant %v", names, want)
	}
}

// 反向守：不带 schema 的普通对话必须还是逐字增量，
// 不能因为这次改动把所有流式响应都变成「攒完再吐」。
func TestResponsesStreamSeparatesMessageOutputItems(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`, `data: {"response":{"id":"r"}}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"message"}}`, ``,
		`event: response.output_text.delta`, `data: {"delta":"one"}`, ``,
		`event: response.output_item.done`, `data: {"item":{"type":"message"}}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"message"}}`, ``,
		`event: response.output_text.delta`, `data: {"delta":"two"}`, ``,
		`event: response.output_item.done`, `data: {"item":{"type":"message"}}`, ``,
		`event: response.completed`, `data: {"response":{"usage":{}}}`, ``,
	}, "\n")
	raw, err := io.ReadAll(newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", false))
	if err != nil {
		t.Fatal(err)
	}
	var starts, deltas, stops []int
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := gjson.Parse(strings.TrimPrefix(line, "data: "))
		switch d.Get("type").String() {
		case "content_block_start":
			starts = append(starts, int(d.Get("index").Int()))
		case "content_block_delta":
			deltas = append(deltas, int(d.Get("index").Int()))
		case "content_block_stop":
			stops = append(stops, int(d.Get("index").Int()))
		}
	}
	if !reflect.DeepEqual(starts, []int{0, 1}) || !reflect.DeepEqual(deltas, []int{0, 1}) || !reflect.DeepEqual(stops, []int{0, 1}) {
		t.Fatalf("message block indexes start=%v delta=%v stop=%v", starts, deltas, stops)
	}
}

func TestResponsesStreamClosesTextBeforeToolBlock(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`, `data: {"response":{"id":"r"}}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"message","id":"m"}}`, ``,
		`event: response.output_text.delta`, `data: {"delta":"text"}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","name":"f"}}`, ``,
		`event: response.output_item.done`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","arguments":"{}"}}`, ``,
		`event: response.completed`, `data: {"response":{"usage":{}}}`, ``,
	}, "\n")
	raw, err := io.ReadAll(newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", false))
	if err != nil {
		t.Fatal(err)
	}
	textStop := strings.Index(string(raw), `"index":0,"type":"content_block_stop"`)
	toolStart := strings.Index(string(raw), `"index":1,"type":"content_block_start"`)
	if textStop < 0 || toolStart < 0 || textStop > toolStart {
		t.Fatalf("text block not closed before tool block: %s", raw)
	}
}

func TestResponsesStreamDoneCarriesFunctionArguments(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`, `data: {"response":{"id":"r"}}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","name":"f"}}`, ``,
		`event: response.output_item.done`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","arguments":"{\"x\":1}"}}`, ``,
		`event: response.completed`, `data: {"response":{"usage":{}}}`, ``,
	}, "\n")
	raw, err := io.ReadAll(newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"partial_json":"{\"x\":1}"`) {
		t.Fatalf("missing done arguments: %s", raw)
	}
}

// Chat streams also need to recover complete arguments carried by output_item.done.
func TestResponsesChatStreamDoneCarriesFunctionArguments(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`, `data: {"response":{"id":"r"}}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","name":"f"}}`, ``,
		`event: response.output_item.done`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","arguments":"{\"x\":1}"}}`, ``,
		`event: response.completed`, `data: {"response":{"usage":{}}}`, ``,
	}, "\n")
	raw, err := io.ReadAll(newResponsesChatStream(io.NopCloser(strings.NewReader(upstream)), "m", false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `\"x\":1`) {
		t.Fatalf("missing done arguments: %s", raw)
	}
}

func TestStreamStaysIncrementalWithoutSchema(t *testing.T) {
	upstream := structuredUpstream("你好呀")
	s := newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", false)
	raw, _ := io.ReadAll(s)
	text, names := sseTexts(string(raw))
	if text != "你好呀" {
		t.Errorf("文本 = %q", text)
	}
	if n := strings.Count(strings.Join(names, ","), "content_block_delta"); n != 3 {
		t.Errorf("content_block_delta = %d 个，应逐字发出 3 个", n)
	}
}

// 结构化请求但一个字都没产出（只有 reasoning，或被 max_output_tokens 截断）时，
// 不能留下空文本块——那个块会进历史，之后每轮回传都换来
// 400 text content blocks must be non-empty。
func TestStreamStructuredNoTextEmitsNoBlock(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		``,
	}, "\n")
	shape := structuredNullShapeFromSchema(`{"type":"object","properties":{"achieved":{"type":"boolean"},"blockedBy":{"type":"string"},"impossible":{"type":"boolean"}},"required":["achieved"]}`)
	s := newResponsesStream(io.NopCloser(strings.NewReader(upstream)), "m", true, shape)
	raw, _ := io.ReadAll(s)
	if strings.Contains(string(raw), "content_block_start") {
		t.Error("没有内容却开了文本块")
	}
}

// chat/completions 方向同一个缺陷，一并守住。
func TestChatStreamStripsNulls(t *testing.T) {
	upstream := structuredUpstream(`{"achieved":true,"reason":null}`)
	shape := structuredNullShapeFromSchema(`{"type":"object","properties":{"achieved":{"type":"boolean"},"reason":{"type":"string"}},"required":["achieved"]}`)
	s := newResponsesChatStream(io.NopCloser(strings.NewReader(upstream)), "m", true, shape)
	raw, _ := io.ReadAll(s)

	var text strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		text.WriteString(gjson.Parse(strings.TrimPrefix(line, "data: ")).
			Get("choices.0.delta.content").String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text.String()), &got); err != nil {
		t.Fatalf("产出不是合法 JSON: %q", text.String())
	}
	if _, ok := got["reason"]; ok {
		t.Errorf("null 字段没摘掉: %q", text.String())
	}
	if got["achieved"] != true {
		t.Errorf("非 null 的值被改动了: %v", got["achieved"])
	}
}

func TestResponsesRefusalStreamsPreserved(t *testing.T) {
	up := strings.Join([]string{
		`event: response.created`, `data: {"response":{"id":"r"}}`, ``,
		`event: response.output_item.added`, `data: {"item":{"type":"message"}}`, ``,
		`event: response.refusal.delta`, `data: {"delta":"cannot "}`, ``,
		`event: response.refusal.delta`, `data: {"delta":"comply"}`, ``,
		`event: response.refusal.done`, `data: {"refusal":"cannot comply"}`, ``,
		`event: response.output_item.done`, `data: {"item":{"type":"message"}}`, ``,
		`event: response.completed`, `data: {"response":{"status":"completed","usage":{}}}`, ``,
	}, "\n")
	a, _ := io.ReadAll(newResponsesStream(io.NopCloser(strings.NewReader(up)), "m", false))
	text, _ := sseTexts(string(a))
	if text != "cannot comply" || !strings.Contains(string(a), `"stop_reason":"refusal"`) {
		t.Fatalf("Anthropic refusal stream lost: %s", a)
	}
	c, _ := io.ReadAll(newResponsesChatStream(io.NopCloser(strings.NewReader(up)), "m", false))
	var refusal strings.Builder
	for _, line := range strings.Split(string(c), "\n") {
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
			refusal.WriteString(gjson.Parse(strings.TrimPrefix(line, "data: ")).Get("choices.0.delta.refusal").String())
		}
	}
	if refusal.String() != "cannot comply" || !strings.Contains(string(c), `"finish_reason":"content_filter"`) {
		t.Fatalf("Chat refusal stream lost: %s", c)
	}
}

func TestResponsesStreamFailedIsError(t *testing.T) {
	up := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\nevent: response.failed\ndata: {\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"bad upstream\"}}}\n\n"
	raw, _ := io.ReadAll(newResponsesStream(io.NopCloser(strings.NewReader(up)), "m", false))
	got := string(raw)
	if !strings.Contains(got, "event: error") || strings.Contains(got, "message_stop") {
		t.Fatalf("failed mapping: %s", got)
	}
	if !strings.Contains(got, "bad upstream") {
		t.Fatalf("safe error message missing: %s", got)
	}
}

func TestResponsesIncompleteReasons(t *testing.T) {
	for _, tc := range []struct {
		reason string
		ok     bool
	}{{"max_output_tokens", true}, {"max_tool_calls", false}, {"content_filter", false}} {
		body := `{"status":"incomplete","incomplete_details":{"reason":"` + tc.reason + `"},"output":[]}`
		anthropic, aerr := responsesToAnthropic([]byte(body), "m")
		chat, cerr := responsesToChat([]byte(body), "m")
		if tc.ok {
			if aerr != nil || gjson.GetBytes(anthropic, "stop_reason").String() != "max_tokens" {
				t.Errorf("%s: Anthropic=%s err=%v", tc.reason, anthropic, aerr)
			}
			if cerr != nil || gjson.GetBytes(chat, "choices.0.finish_reason").String() != "length" {
				t.Errorf("%s: Chat=%s err=%v", tc.reason, chat, cerr)
			}
		} else if aerr == nil || cerr == nil {
			t.Errorf("%s must remain an upstream error: Anthropic=%v Chat=%v", tc.reason, aerr, cerr)
		}
	}
}

func TestResponsesStreamArgumentsDoneCompletesMissingDelta(t *testing.T) {
	up := strings.Join([]string{`event: response.created`, `data: {"response":{"id":"r"}}`, ``, `event: response.output_item.added`, `data: {"item":{"type":"function_call","id":"i","call_id":"c","name":"f"}}`, ``, `event: response.function_call_arguments.done`, `data: {"item_id":"i","arguments":"{\"x\":1}"}`, ``, `event: response.completed`, `data: {"response":{"status":"completed"}}`, ``}, "\n")
	raw, _ := io.ReadAll(newResponsesStream(io.NopCloser(strings.NewReader(up)), "m", false))
	var args string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "data: ") {
			args += gjson.Parse(strings.TrimPrefix(line, "data: ")).Get("delta.partial_json").String()
		}
	}
	if args != `{"x":1}` {
		t.Fatalf("done arguments = %q", args)
	}
}
