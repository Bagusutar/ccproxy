package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testTargetRequest(t *testing.T, method, path, body string, target *target) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "http://proxy"+path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxTarget, target))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	return req
}

func TestXlateResponsesRejectsOversizeHTMLAndTruncated(t *testing.T) {
	p := &Proxy{logf: log.New(io.Discard, "", 0)}
	cases := []struct {
		name string
		body []byte
	}{
		{"html", []byte("<html>gateway error</html>")},
		{"truncated", []byte(`{"id":"r","output":[`)},
		{"oversize", append([]byte(`{"id":"r","output":[],"padding":"`), append([]byte(strings.Repeat("x", 32<<20)), []byte(`"}`)...)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(tc.body))}
			target := &target{client: ProtoAnthropic}
			if err := p.xlateResponses(resp, target); err == nil {
				t.Fatal("invalid/oversized response must fail translation")
			}
		})
	}
}

func TestResponsesTranslationRejectsInvalidJSON(t *testing.T) {
	for _, fn := range []func([]byte, string) ([]byte, error){responsesToAnthropic, responsesToChat} {
		if _, err := fn([]byte(`{"output":[`), "m"); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}

func TestStructuredFallbackDecodeFailureRecordsUsageAndRestoresExact(t *testing.T) {
	const original = `{"type":"error","error":{"message":"original"},"usage":{"input_tokens":7,"output_tokens":3}}`
	const fallback = `{"type":"message","content":[{"type":"text","text":"not a tool"}],"usage":{"input_tokens":11,"output_tokens":5}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"output_config"`)) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, fallback)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, original)
	}))
	defer up.Close()
	body := `{"model":"m","messages":[],"output_config":{"format":{"schema":{"type":"object"}}}}`
	req := httptest.NewRequest(http.MethodPost, up.URL+"/v1/messages", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxTarget, &target{id: "p", name: "p", model: "m"}))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	plan := planStructuredFallback("/v1/messages", []byte(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxStructured, plan))
	meter := newUsageMeter()
	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0), meter: meter}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != original {
		t.Fatalf("restored body changed: %q", got)
	}
	rows := meter.snapshot().Rows
	if len(rows) != 1 || rows[0].In != 11 || rows[0].Out != 5 || rows[0].Reqs != 1 {
		t.Fatalf("usage=%+v", rows)
	}
}

func TestRetryDiscardedUsageAndFinalTapAreSeparate(t *testing.T) {
	var n int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"retry","usage":{"input_tokens":4,"output_tokens":2}}`)
			return
		}
		_, _ = io.WriteString(w, `{"type":"message","usage":{"input_tokens":9,"output_tokens":6},"content":[]}`)
	}))
	defer up.Close()
	tg := &target{id: "p", name: "p", model: "m"}
	req := httptest.NewRequest(http.MethodPost, up.URL+"/v1/messages", strings.NewReader(`{"messages":[]}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxTarget, tg))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"messages":[]}`)), nil }
	meter := newUsageMeter()
	tr := &retryTransport{base: &http.Transport{}, logf: log.New(io.Discard, "", 0), meter: meter}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	in, _, _, out := usageFromBody(raw)
	meter.Add(tg.id, tg.name, tg.model, in, 0, 0, out)
	rows := meter.snapshot().Rows
	if len(rows) != 1 || rows[0].Reqs != 2 || rows[0].In != 13 || rows[0].Out != 8 {
		t.Fatalf("rows=%+v", rows)
	}
}
