package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIUsageHasIndependentInclusiveUsageAndMeterRanges(t *testing.T) {
	claude := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claude, "projects", "p"), 0700); err != nil {
		t.Fatal(err)
	}
	usagePath := filepath.Join(claude, "projects", "p", "s.jsonl")
	writeUsageLine(t, usagePath, "u1", "m", "2026-01-01T00:00:00Z", 1, 1)
	writeUsageLine(t, usagePath, "u2", "m", "2026-01-02T00:00:00Z", 2, 2)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	sandboxDataDir(t)
	meter := newUsageMeter()
	meter.AddAt(dateUTC(2026, 1, 1), "p", "P", "m", 10, 0, 0, 1)
	meter.AddAt(dateUTC(2026, 1, 2), "p", "P", "m", 20, 0, 0, 2)
	meter.Flush()

	srv := &UIServer{usage: newUsageScanner()}
	call := func(query string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodGet, "/api/usage?"+query, nil)
		w := httptest.NewRecorder()
		srv.handleUsage(w, r)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return w.Code, body
	}
	if code, body := call("usageFrom=2026-01-01&usageTo=2026-01-01&meterFrom=2026-01-02&meterTo=2026-01-02"); code != http.StatusOK {
		t.Fatalf("independent ranges status = %d body=%v", code, body)
	} else {
		usage := body["usage"].(map[string]any)
		if usage["total"].(map[string]any)["in"].(float64) != 1 {
			t.Fatalf("usage range leaked = %v", usage)
		}
		rows := body["meter"].(map[string]any)["rows"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["in"].(float64) != 20 {
			t.Fatalf("meter range leaked = %v", rows)
		}
	}
	for _, query := range []string{
		"usageFrom=2026-02-02&usageTo=2026-01-01",
		"usageFrom=2026-01-02",
		"meterTo=2026-01-02",
		"meterFrom=bad&meterTo=",
		"meterFrom=2026-01-02&meterTo=2026-01-01",
	} {
		if code, _ := call(query); code != http.StatusBadRequest {
			t.Errorf("%q status = %d, want 400", query, code)
		}
	}
}

func TestFailSavedResponseContract(t *testing.T) {
	w := httptest.NewRecorder()
	failSaved(w, 15723, errors.New("boom"))
	var body struct {
		OK    bool   `json:"ok"`
		Saved bool   `json:"saved"`
		Port  int    `json:"port"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || !body.Saved || body.Port != 15723 {
		t.Fatalf("response = %+v", body)
	}
	if body.Error != "配置已保存，但代理未能启动：boom" {
		t.Fatalf("error = %q", body.Error)
	}
}

func TestEmbeddedUISavedFailureSemantics(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(js)
	for _, want := range []string{
		"if (r.saved)",
		"title: '配置已保存，但代理未启动'",
		"state.port = r.port",
		"state.dirty = false",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("embedded UI lacks %q", want)
		}
	}
}

func TestApplySaveRuntimeConfigPreservesOmittedValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Port, cfg.FirstByteSec, cfg.StallSec = 32123, 12, 34
	applySaveRuntimeConfig(cfg, saveReq{})
	if cfg.Port != 32123 || cfg.FirstByteSec != 12 || cfg.StallSec != 34 {
		t.Fatalf("omitted values changed config: %+v", cfg)
	}

	applySaveRuntimeConfig(cfg, saveReq{Port: 43210, FirstByteSec: maxTimeoutSeconds + 1, StallSec: -1})
	if err := validateConfig(cfg); err == nil {
		t.Fatal("invalid provided timeout values were accepted")
	}
}

func TestResetUsage(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		wantErr bool
	}{
		{"200 succeeds", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), false},
		{"non-200 fails", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }), true},
		{"timeout fails", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { time.Sleep(100 * time.Millisecond) }), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			err := resetUsage(&http.Client{Timeout: 20 * time.Millisecond}, srv.URL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	if err := resetUsage(&http.Client{Timeout: 20 * time.Millisecond}, "http://127.0.0.1:1/reset"); err == nil {
		t.Fatal("connection error should fail")
	}
}

func dateUTC(y int, m int, d int) time.Time {
	return time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC)
}
