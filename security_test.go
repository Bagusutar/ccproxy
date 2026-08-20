package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func securityTestProxy(t *testing.T, upstream http.Handler) (*Proxy, *atomic.Int32, *bytes.Buffer) {
	t.Helper()
	sandboxDataDir(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := DefaultConfig()
	cfg.Providers[0].BaseURL = srv.URL
	cfg.Providers[0].Token = "test-token"
	var logs bytes.Buffer
	return NewProxy(cfg, log.New(&logs, "", 0)), &hits, &logs
}

func TestProxyRejectsExplicitCrossSiteBrowserRequests(t *testing.T) {
	p, hits, _ := securityTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	body := `{"model":"m","messages":[]}`
	tests := []struct {
		name, origin, fetchSite string
		want                    int
	}{
		{name: "evil origin", origin: "https://evil.example", want: http.StatusForbidden},
		{name: "cross-site fetch metadata", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "absent browser headers", want: http.StatusOK},
		{name: "local origin", origin: "http://127.0.0.1:15722", fetchSite: "same-origin", want: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := hits.Load()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.fetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
			wantHits := before
			if tt.want == http.StatusOK {
				wantHits++
			}
			if hits.Load() != wantHits {
				t.Fatalf("upstream hits = %d, want %d", hits.Load(), wantHits)
			}
		})
	}
}

func TestProxyRejectsOversizedBodyBeforeUpstream(t *testing.T) {
	p, hits, _ := securityTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", io.LimitReader(strings.NewReader(strings.Repeat("x", 1024)), 0))
	r.Body = io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("x", 1<<20)), strings.NewReader(strings.Repeat("x", requestBodyCap-(1<<20)+1))))
	r.ContentLength = -1 // exercise the streaming/chunked limit, not only the header fast path
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("oversized request reached upstream %d times", hits.Load())
	}
}

func TestUpstreamErrorBodyNeverEntersLogsOrDiagnostics(t *testing.T) {
	const sentinel = "TASK44-SENTINEL-DO-NOT-LEAK"
	p, _, logs := securityTestProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, sentinel)
	}))
	front := httptest.NewServer(p)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(got), sentinel) {
		t.Fatal("client did not receive the unchanged upstream error body")
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Fatal("upstream error body leaked into logs")
	}

	cfg := DefaultConfig()
	cfg.Providers[0].Token = sentinel
	lp, err := logPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("raw log "+sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(&Status{PID: 1, Port: cfg.Port, UpdatedAt: time.Now(), LastError: sentinel}); err != nil {
		t.Fatal(err)
	}
	if got := diagnostics(cfg); strings.Contains(got, sentinel) {
		t.Fatal("copyable diagnostics leaked sentinel")
	}
}
