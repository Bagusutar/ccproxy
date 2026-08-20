package main

import (
	"encoding/json"
	"testing"
)

// 探测结论必须能原样写进配置再读回来——它的全部价值就在于「测一次，之后一直用」，
// 任何一处序列化丢字段都会让每次启动重新探测。
func TestModelProtocolsRoundTrip(t *testing.T) {
	in := Config{Providers: []Provider{{
		ID: "p1", BaseURL: "https://gw.example.com",
		ModelProtocols: map[string][]string{
			"claude-opus-5": {"anthropic", "chat"},
			"gpt-5.6-luna":  {"responses"},
		},
	}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	p := &out.Providers[0]
	if !p.SupportsProtocol("gpt-5.6-luna", ProtoResponses) {
		t.Errorf("往返后丢了 gpt-5.6-luna 的方言: %s", raw)
	}
	if !p.SupportsProtocol("claude-opus-5", ProtoChat) {
		t.Errorf("往返后丢了 claude-opus-5 的方言: %s", raw)
	}
}

// 没测过的上游不该在配置里留下空字段。
func TestModelProtocolsOmittedWhenEmpty(t *testing.T) {
	raw, _ := json.Marshal(Provider{ID: "p1", BaseURL: "https://x"})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["modelProtocols"]; ok {
		t.Errorf("未探测时不应写出 modelProtocols: %s", raw)
	}
}

func names(ts []probeTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t.proto) + " -> " + t.url
	}
	return out
}

// 双地址填全时各测各的：拿 Anthropic 地址去试 OpenAI 端点只会得到
// 误导性的 404，把「不支持 OpenAI」这个错误结论写进配置。
func TestPlanProbesDualURL(t *testing.T) {
	got := names(planProbes("https://api.deepseek.com/anthropic", "https://api.deepseek.com"))
	want := []string{
		"anthropic -> https://api.deepseek.com/anthropic/v1/messages",
		"chat -> https://api.deepseek.com/v1/chat/completions",
		"responses -> https://api.deepseek.com/v1/responses",
	}
	if len(got) != len(want) {
		t.Fatalf("目标数 = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 只填一个地址时三种方言都往它上试——多数上游把两套端点挂在同一个根下。
func TestPlanProbesSingleURL(t *testing.T) {
	got := names(planProbes("https://gw.example.com", ""))
	want := []string{
		"anthropic -> https://gw.example.com/v1/messages",
		"chat -> https://gw.example.com/v1/chat/completions",
		"responses -> https://gw.example.com/v1/responses",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// 两个地址填成一样，等同于只填一个，不该出现重复目标。
func TestPlanProbesIdenticalURLs(t *testing.T) {
	got := planProbes("https://gw.example.com/", "https://gw.example.com")
	if len(got) != 3 {
		t.Fatalf("目标数 = %d, want 3: %v", len(got), names(got))
	}
	for _, tg := range got {
		if got0 := "https://gw.example.com" + tg.proto.endpoint(); tg.url != got0 {
			t.Errorf("%s -> %q, want %q", tg.proto, tg.url, got0)
		}
	}
}

func TestPlanProbesEmptyBase(t *testing.T) {
	if got := planProbes("", ""); len(got) != 0 {
		t.Errorf("地址全空时应无探测目标, got %v", names(got))
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://example.com", "https://example.com"},
		{" https://example.com/v1/ ", "https://example.com"},
		{"https://example.com/anthropic/v1", "https://example.com/anthropic"},
	} {
		got, err := normalizeBaseURL(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeBaseURL(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "example.com", "https://example.com?token=x", "https://user:secret@example.com", "https://"} {
		if _, err := normalizeBaseURL(in); err == nil {
			t.Errorf("normalizeBaseURL(%q) should reject", in)
		}
	}
}

func TestPlanProbesAcceptsV1Base(t *testing.T) {
	got := names(planProbes("https://gw.example.com/v1", ""))
	if got[0] != "anthropic -> https://gw.example.com/v1/messages" || got[1] != "chat -> https://gw.example.com/v1/chat/completions" {
		t.Errorf("/v1 base duplicated or altered: %v", got)
	}

	nested := names(planProbes("https://gw.example.com/anthropic/v1", ""))
	if nested[0] != "anthropic -> https://gw.example.com/anthropic/v1/messages" {
		t.Errorf("nested /v1 base duplicated or altered: %v", nested)
	}
}

// 只看 200 不够：网关在方言不匹配时也可能回 200 加一段错误 JSON。
// 三种方言各有一个稳定的顶层自证字段。
func TestProbeAccepts(t *testing.T) {
	cases := []struct {
		name string
		p    Protocol
		body string
		want bool
	}{
		{"anthropic 正常", ProtoAnthropic, `{"type":"message","content":[]}`, true},
		{"anthropic 收到错误体", ProtoAnthropic, `{"type":"error","error":{}}`, false},
		{"chat 正常", ProtoChat, `{"object":"chat.completion","choices":[]}`, true},
		{"chat 收到 responses 的形状", ProtoChat, `{"object":"response"}`, false},
		{"responses 完成", ProtoResponses, `{"object":"response","status":"completed"}`, true},
		// 探测只给 16 个输出 token，推理型模型会在思考阶段用光。
		// incomplete 同样证明端点是通的（实测 deepseek-v4-flash）。
		{"responses 截断也算通", ProtoResponses, `{"object":"response","status":"incomplete"}`, true},
		{"responses 收到 chat 的形状", ProtoResponses, `{"object":"chat.completion"}`, false},
		{"空体", ProtoAnthropic, ``, false},
	}
	for _, c := range cases {
		if got := probeAccepts(c.p, []byte(c.body)); got != c.want {
			t.Errorf("%s: probeAccepts(%s) = %v, want %v", c.name, c.p, got, c.want)
		}
	}
}

// Responses 的最小请求和另外两种不同：它没有 max_tokens/messages。
func TestProbeBodyShape(t *testing.T) {
	if got := string(probeBody(ProtoResponses, "m")); got != `{"input":"hi","max_output_tokens":16,"model":"m"}` {
		t.Errorf("responses 探测体 = %s", got)
	}
	if got := string(probeBody(ProtoChat, "m")); got != `{"max_tokens":1,"messages":[{"content":"hi","role":"user"}],"model":"m"}` {
		t.Errorf("chat 探测体 = %s", got)
	}
}

func TestProviderProtocolLookup(t *testing.T) {
	p := Provider{ModelProtocols: map[string][]string{
		"gpt-5.6-luna":  {"responses"},
		"claude-opus-5": {"anthropic", "chat"},
	}}
	if !p.SupportsProtocol("claude-opus-5", ProtoAnthropic) {
		t.Error("claude-opus-5 应支持 anthropic")
	}
	if p.SupportsProtocol("claude-opus-5", ProtoResponses) {
		t.Error("claude-opus-5 不该支持 responses")
	}
	// 槽位里的模型名可能带 [1m] 之类的上下文档位后缀，查表前要剥掉。
	if !p.SupportsProtocol("claude-opus-5[1m]", ProtoAnthropic) {
		t.Error("带档位后缀的模型名应能查到")
	}
	// 未测过 ≠ 不支持，但查表只能如实返回「没有记录」。
	if p.ProtocolsFor("never-tested") != nil {
		t.Error("未测过的模型应返回 nil")
	}
}
