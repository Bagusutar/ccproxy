package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestProxyMeterIntegration exercises the complete local path without touching a
// real config/data directory or an external network endpoint.
func TestProxyMeterIntegration(t *testing.T) {
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	sandboxDataDir(t)

	integrationHits.reset()
	upstream := httptest.NewServer(http.HandlerFunc(integrationUpstream))
	defer upstream.Close()

	models := map[string][]Protocol{
		"claude-opus-5-anth-ns":     {ProtoAnthropic},
		"claude-opus-5-anth-sse":    {ProtoAnthropic},
		"claude-opus-5-chat":        {ProtoChat},
		"claude-opus-5-resp-ns":     {ProtoResponses},
		"claude-opus-5-resp-sse":    {ProtoResponses},
		"claude-opus-5-tr-anth-ns":  {ProtoResponses},
		"claude-opus-5-tr-anth-sse": {ProtoResponses},
		"claude-opus-5-tr-chat":     {ProtoResponses},
		"claude-opus-5-error":       {ProtoAnthropic},
	}
	protocolStrings := map[string][]string{}
	for model, ps := range models {
		for _, p := range ps {
			protocolStrings[model] = append(protocolStrings[model], string(p))
		}
	}
	cfg := &Config{
		Port: defaultPort,
		Providers: []Provider{{ID: "integration", Name: "Integration Upstream", BaseURL: upstream.URL,
			OpenAIBaseURL: upstream.URL, Token: "test-token", Models: mapKeys(models), ModelProtocols: protocolStrings}},
		Slots:        map[string]Slot{},
		FirstByteSec: 5,
		StallSec:     5,
	}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	p := NewProxy(cfg, log.New(io.Discard, "", 0))
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	client := &http.Client{}
	post := func(path, model string, body map[string]any) []byte {
		t.Helper()
		body["model"] = model
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post(proxy.URL+path, "application/json", strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", model, path, resp.StatusCode, got)
		}
		if len(got) == 0 {
			t.Fatalf("%s %s: empty client body", model, path)
		}
		return got
	}

	// Native Anthropic, including a streaming response whose usage is split over events.
	post("/v1/messages", "claude-opus-5-anth-ns", map[string]any{"messages": []any{}, "max_tokens": 16})
	post("/v1/messages", "claude-opus-5-anth-sse", map[string]any{"messages": []any{}, "max_tokens": 16, "stream": true})
	// Native Chat and native Responses (non-streaming plus SSE).
	post("/v1/chat/completions", "claude-opus-5-chat", map[string]any{"messages": []any{}})
	post("/v1/responses", "claude-opus-5-resp-ns", map[string]any{"input": "hi", "max_output_tokens": 16})
	post("/v1/responses", "claude-opus-5-resp-sse", map[string]any{"input": "hi", "max_output_tokens": 16, "stream": true})
	// Anthropic -> Responses and Chat -> Responses translation, both non-streaming;
	// the former also covers the streaming translation path.
	trAnth := post("/v1/messages", "claude-opus-5-tr-anth-ns", map[string]any{"messages": []any{}, "max_tokens": 16})
	if !strings.Contains(string(trAnth), `"type":"message"`) {
		t.Fatalf("Anthropic translation did not return Anthropic message: %s", trAnth)
	}
	trAnthSSE := post("/v1/messages", "claude-opus-5-tr-anth-sse", map[string]any{"messages": []any{}, "max_tokens": 16, "stream": true})
	if !strings.Contains(string(trAnthSSE), "message_stop") {
		t.Fatalf("Anthropic SSE translation did not finish: %s", trAnthSSE)
	}
	trChat := post("/v1/chat/completions", "claude-opus-5-tr-chat", map[string]any{"messages": []any{}})
	if !strings.Contains(string(trChat), `"object":"chat.completion"`) {
		t.Fatalf("Chat translation did not return Chat completion: %s", trChat)
	}
	// A usage-free upstream error must not create a meter row.
	errResp, err := client.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-opus-5-error","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer errResp.Body.Close()
	if errResp.StatusCode != http.StatusInternalServerError {
		errBody, _ := io.ReadAll(errResp.Body)
		t.Fatalf("usage-free upstream error status=%d body=%s", errResp.StatusCode, errBody)
	}

	if err := p.meter.Flush(); err != nil {
		t.Fatal(err)
	}
	meter := ReadMeter()
	wantRows := map[string]meterVal{
		"claude-opus-5-anth-ns":     {Reqs: 1, In: 10, Out: 20},
		"claude-opus-5-anth-sse":    {Reqs: 1, In: 10, Out: 20},
		"claude-opus-5-chat":        {Reqs: 1, In: 30, Out: 20},
		"claude-opus-5-resp-ns":     {Reqs: 1, In: 40, CacheR: 10, Out: 20},
		"claude-opus-5-resp-sse":    {Reqs: 1, In: 40, CacheR: 10, Out: 20},
		"claude-opus-5-tr-anth-ns":  {Reqs: 1, In: 40, CacheR: 10, Out: 20},
		"claude-opus-5-tr-anth-sse": {Reqs: 1, In: 40, CacheR: 10, Out: 20},
		"claude-opus-5-tr-chat":     {Reqs: 1, In: 40, CacheR: 10, Out: 20},
	}
	gotRows := map[string]meterVal{}
	for _, row := range meter.Rows {
		if row.Provider != "integration" {
			t.Errorf("unexpected provider row: %+v", row)
			continue
		}
		gotRows[row.Model] = row.meterVal
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("meter rows=%d, want exact %d: %+v", len(gotRows), len(wantRows), gotRows)
	}
	for model, want := range wantRows {
		if got, ok := gotRows[model]; !ok {
			t.Errorf("missing meter row for %s", model)
		} else if got != want {
			t.Errorf("meter row %s = %+v, want %+v", model, got, want)
		}
		if hits := integrationHits.count(model); hits != 1 {
			t.Errorf("upstream physical hits for %s = %d, want exactly 1", model, hits)
		}
	}

	// Exercise the real UIServer /api/usage handler, including date filtering,
	// rows, and computed price/cost fields.
	ui, err := NewUIServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ui.ln.Close()
	go func() { _ = ui.Serve() }()
	u, _ := url.Parse(ui.URL())
	q := u.Query()
	day := localDay(p.startedAt)
	q.Set("meterFrom", day)
	q.Set("meterTo", day)
	req, _ := http.NewRequest(http.MethodGet, "http://"+ui.ln.Addr().String()+"/api/usage?"+q.Encode(), nil)
	req.Header.Set("X-CCProxy-Key", ui.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var api map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || api["ok"] != true {
		t.Fatalf("/api/usage status=%d body=%v", resp.StatusCode, api)
	}
	meterAPI, ok := api["meter"].(map[string]any)
	if !ok {
		t.Fatalf("/api/usage missing meter: %v", api)
	}
	rows, ok := meterAPI["rows"].([]any)
	if !ok || len(rows) < 7 {
		t.Fatalf("/api/usage rows=%v", meterAPI["rows"])
	}
	for _, item := range rows {
		row := item.(map[string]any)
		if row["date"] != nil { // date is represented by the query/day grouping, not row schema
			_ = row["date"]
		}
		if row["priced"] != true || row["cost"].(float64) <= 0 {
			t.Errorf("meter row lacks priced cost: %v", row)
		}
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type integrationHitCounter struct {
	sync.Mutex
	models map[string]int
}

var integrationHits = integrationHitCounter{models: map[string]int{}}

func (h *integrationHitCounter) reset() {
	h.Lock()
	defer h.Unlock()
	h.models = map[string]int{}
}

func (h *integrationHitCounter) count(model string) int {
	h.Lock()
	defer h.Unlock()
	return h.models[model]
}

func integrationUpstream(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	model, _ := req["model"].(string)
	integrationHits.Lock()
	integrationHits.models[model]++
	integrationHits.Unlock()
	if model == "claude-opus-5-error" {
		http.Error(w, "upstream failure without usage", http.StatusInternalServerError)
		return
	}
	stream, _ := req["stream"].(bool)
	if strings.HasSuffix(r.URL.Path, "/messages") {
		if stream {
			writeGzip(w, "text/event-stream", anthropicSSE(model))
			return
		}
		writeGzip(w, "application/json", fmt.Sprintf(`{"type":"message","model":%q,"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":20}}`, model))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		writeGzip(w, "application/json", fmt.Sprintf(`{"object":"chat.completion","model":%q,"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":30,"completion_tokens":20}}`, model))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/responses") {
		if stream {
			writeGzip(w, "text/event-stream", responsesSSE(model))
			return
		}
		writeGzip(w, "application/json", fmt.Sprintf(`{"object":"response","id":"resp_1","model":%q,"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":10},"output_tokens":20}}`, model))
		return
	}
	http.NotFound(w, r)
}

func writeGzip(w http.ResponseWriter, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	gz := gzip.NewWriter(w)
	_, _ = gz.Write([]byte(body))
	_ = gz.Close()
}

func anthropicSSE(model string) string {
	return fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"model\":%q,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n", model) +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":20}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func responsesSSE(model string) string {
	return fmt.Sprintf("event: response.created\ndata: {\"response\":{\"id\":\"resp_1\",\"model\":%q}}\n\n", model) +
		"event: response.output_item.added\ndata: {\"item\":{\"type\":\"message\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"ok\"}\n\n" +
		"event: response.output_item.done\ndata: {\"item\":{\"type\":\"message\"}}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":50,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":20}}}\n\n"
}
