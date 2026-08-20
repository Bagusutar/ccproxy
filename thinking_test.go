package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeDropsThinking(t *testing.T) {
	in := `{"model":"claude-opus-5","thinking":{"type":"adaptive"},"messages":[
	  {"role":"user","content":"问题"},
	  {"role":"assistant","content":[
	     {"type":"thinking","thinking":"想了想","signature":"695f9830-2307"},
	     {"type":"text","text":"答案"}]},
	  {"role":"user","content":[{"type":"text","text":"继续"}]}]}`

	out, _, changed := sanitizeHistory([]byte(in), true, nil)
	if !changed {
		t.Fatal("应当识别出 thinking 块并改写")
	}
	root := gjson.ParseBytes(out)

	// 顶层的 thinking 配置是「本次要不要思考」，与历史签名无关，删了会把功能关掉。
	if got := root.Get("thinking.type").String(); got != "adaptive" {
		t.Errorf("顶层 thinking 配置被误删: %s", root.Get("thinking").Raw)
	}
	if n := len(root.Get("messages").Array()); n != 3 {
		t.Fatalf("消息数 = %d，不该丢消息", n)
	}
	blocks := root.Get("messages.1.content").Array()
	if len(blocks) != 1 || blocks[0].Get("type").String() != "text" {
		t.Errorf("助手轮 = %s", root.Get("messages.1.content").Raw)
	}
	if got := blocks[0].Get("text").String(); got != "答案" {
		t.Errorf("文本块被动过: %q", got)
	}
	// 字符串形态的 content 不该被改成数组。
	if root.Get("messages.0.content").IsArray() {
		t.Errorf("字符串 content 被改写了: %s", root.Get("messages.0.content").Raw)
	}
}

// 内容被删空的消息必须整条丢弃：content 为空数组是非法的，
// 留着只会换来另一种 400。
func TestSanitizeDropsEmptiedMessage(t *testing.T) {
	in := `{"messages":[
	  {"role":"user","content":"问题"},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"只想不说","signature":"x"}]},
	  {"role":"user","content":"继续"}]}`
	out, _, changed := sanitizeHistory([]byte(in), true, nil)
	if !changed {
		t.Fatal("应当改写")
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，被删空的那条应当整体丢弃: %s", len(msgs),
			gjson.GetBytes(out, "messages").Raw)
	}
	if msgs[1].Get("content").String() != "继续" {
		t.Errorf("剩下的消息不对: %s", msgs[1].Raw)
	}
}

func TestSanitizeNoopWhenClean(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"你好"}]}`
	if _, _, changed := sanitizeHistory([]byte(in), true, nil); changed {
		t.Error("没有 thinking 块时不该改写")
	}
	// 顶层配置本身不构成改写理由。
	in2 := `{"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"你好"}]}`
	if _, _, changed := sanitizeHistory([]byte(in2), true, nil); changed {
		t.Error("只有顶层 thinking 配置时不该改写")
	}
}

// 端到端：模仿网关的行为——带 thinking 块就拒，剥掉就放行。
// 客户端不该看到那个 400。
func TestThinkingFallbackEndToEnd(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hasThinking := false
		gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
			m.Get("content").ForEach(func(_, b gjson.Result) bool {
				if b.Get("type").String() == "thinking" {
					hasThinking = true
				}
				return true
			})
			return true
		})
		w.Header().Set("Content-Type", "application/json")
		if hasThinking {
			seen = append(seen, "rejected")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w,
				`{"type":"error","error":{"message":"content.0: Invalid `+"`signature`"+` in `+"`thinking`"+` block"}}`)
			return
		}
		seen = append(seen, "accepted")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
		  "content":[{"type":"text","text":"好的"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	body := `{"model":"claude-opus-5","messages":[
	  {"role":"user","content":"问题"},
	  {"role":"assistant","content":[
	     {"type":"thinking","thinking":"别家产的","signature":"695f9830-2307"},
	     {"type":"text","text":"答案"}]},
	  {"role":"user","content":"继续"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}

	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0)}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端仍然看到 %d —— 补救的全部意义就是不让这个 400 冒出去", resp.StatusCode)
	}
	if strings.Join(seen, ",") != "rejected,accepted" {
		t.Errorf("上游收到的请求序列 = %v", seen)
	}
	if resp.Request == nil {
		t.Error("resp.Request 必须补上，ModifyResponse 靠它的 context 取路由目标")
	}
}

// 另有原因的 400 不该被这条补救吞掉：重发同样失败时，
// 客户端必须原样收到最初那个错误。
func TestThinkingFallbackKeepsOriginalErrorOnFailure(t *testing.T) {
	const orig = `{"type":"error","error":{"message":"max_tokens too large"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, orig)
	}))
	defer srv.Close()

	body := `{"model":"m","max_tokens":99999999,"messages":[
	  {"role":"assistant","content":[
	     {"type":"thinking","thinking":"x","signature":"s"},{"type":"text","text":"y"}]}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
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
	got, _ := io.ReadAll(resp.Body)
	if string(got) != orig {
		t.Errorf("原始错误体应当完整保留，实际 = %q", got)
	}
}

func TestThinkingFallbackMetersHugeFailedFallback(t *testing.T) {
	const original = `{"type":"error","error":{"message":"invalid signature"}}`
	body := `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"s"},{"type":"text","text":"y"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req = req.WithContext(context.WithValue(req.Context(), ctxTarget, &target{id: "p", name: "provider", model: "m"}))
	resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(original)), Header: make(http.Header)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeHugeFallback(w, http.StatusBadRequest, `{"usage":{"input_tokens":41,"output_tokens":13}}`)
	}))
	defer srv.Close()
	req.URL, _ = req.URL.Parse(srv.URL + "/v1/messages")
	meter := newUsageMeter()
	tr := &retryTransport{meter: meter, logf: quietLogger(), base: &http.Transport{}}
	if got := tr.thinkingFallback(req, resp); got != nil {
		t.Fatal("failed fallback should retain original response")
	}
	rows := meter.snapshot().Rows
	if len(rows) != 1 || rows[0].In != 41 || rows[0].Out != 13 {
		t.Fatalf("meter rows = %+v", rows)
	}
}

// 空文本块是确定的：对 Anthropic 永远非法，与目标上游无关，
// 所以无条件预剥，不需要先撞一次 400。
func TestPresanitizeDropsEmptyTextAlways(t *testing.T) {
	in := `{"messages":[
	  {"role":"user","content":"问题"},
	  {"role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"答案"}]}]}`
	out, changed := presanitize([]byte(in), "p1")
	if !changed {
		t.Fatal("空文本块应当被无条件摘掉")
	}
	blocks := gjson.GetBytes(out, "messages.1.content").Array()
	if len(blocks) != 1 || blocks[0].Get("text").String() != "答案" {
		t.Errorf("content = %s", gjson.GetBytes(out, "messages.1.content").Raw)
	}
}

// thinking 签名不确定来源，所以未被拒过的一律保留——
// 单上游会话里 thinking 是有价值的跨轮推理上下文，不该无差别丢弃。
func TestPresanitizeKeepsUnknownSignatures(t *testing.T) {
	in := `{"messages":[{"role":"assistant","content":[
	  {"type":"thinking","thinking":"想","signature":"未知签名"},
	  {"type":"text","text":"答案"}]}]}`
	if _, changed := presanitize([]byte(in), "p1"); changed {
		t.Error("没被拒过的签名不该预剥")
	}

	// 记入黑名单之后，同一个上游确定性预剥；别的上游不受影响。
	rejectedSigs.reject("p1", []string{"未知签名"})
	t.Cleanup(func() {
		rejectedSigs.set = map[string]struct{}{}
		rejectedSigs.providers = map[string]struct{}{}
	})

	out, changed := presanitize([]byte(in), "p1")
	if !changed {
		t.Fatal("已知被拒的签名应当预剥")
	}
	if gjson.GetBytes(out, "messages.0.content").Array()[0].Get("type").String() != "text" {
		t.Errorf("content = %s", gjson.GetBytes(out, "messages.0.content").Raw)
	}
	if _, changed := presanitize([]byte(in), "p2"); changed {
		t.Error("黑名单是按上游分的，不该影响别的上游")
	}
}

// 干净请求必须原样返回同一块内存——只要发生了重建，就是把整个消息数组
// 重新序列化了一遍。上下文几 MB 时那是每请求几十毫秒的纯浪费，
// 而这条路径每个请求都要走。曾经因为预筛用 `"thinking"` 做关键词
// （顶层 thinking 配置几乎每个请求都带）而实测白花 3.4 毫秒。
func TestPresanitizeDoesNotRebuildCleanBody(t *testing.T) {
	body := []byte(`{"model":"m","thinking":{"type":"adaptive"},"messages":[
	  {"role":"user","content":[{"type":"text","text":"你好"}]},
	  {"role":"assistant","content":[
	     {"type":"thinking","thinking":"想","signature":"合法签名"},
	     {"type":"text","text":"答案"}]}]}`)
	out, changed := presanitize(body, "p1")
	if changed {
		t.Fatal("干净请求不该被改写")
	}
	if &out[0] != &body[0] {
		t.Error("返回了新切片，说明白白重建了一次消息数组")
	}
}

func BenchmarkPresanitizeCleanBody(b *testing.B) {
	var msgs []any
	filler := strings.Repeat("这是一段很长的对话内容。", 200)
	for i := 0; i < 60; i++ {
		msgs = append(msgs, map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "text", "text": filler}}})
	}
	body, _ := json.Marshal(map[string]any{"model": "m",
		"thinking": map[string]any{"type": "adaptive"}, "messages": msgs})
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		presanitize(body, "p1")
	}
}
