package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestPriceValidation(t *testing.T) {
	valid := []Price{{}, {In: 0, CacheW: 1, CacheR: 0, Out: 0, Cur: "CNY"}}
	for _, p := range valid {
		if err := p.validate(); err != nil {
			t.Errorf("valid price rejected: %+v: %v", p, err)
		}
	}
	for _, p := range []Price{{In: -1}, {CacheW: math.NaN()}, {CacheR: math.Inf(1)}, {Out: math.Inf(-1)}, {Cur: "EUR"}} {
		if err := p.validate(); err == nil {
			t.Errorf("invalid price accepted: %+v", p)
		}
	}
	if err := validatePrices(map[string]Price{" claude-opus-5 ": {In: 1}}); err == nil {
		t.Error("model name with surrounding whitespace accepted")
	}
}

func TestCustomZeroPriceDoesNotFallbackToPreset(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","message":{"id":"z","model":"claude-opus-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), map[string]Price{"claude-opus-5": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Models) != 1 {
		t.Fatalf("models = %+v", rep.Models)
	}
	m := rep.Models[0]
	if !m.Priced || !m.Custom || m.Cost != 0 {
		t.Fatalf("explicit zero custom price = %+v", m)
	}
	if rep.Unpriced != 0 {
		t.Fatalf("unpriced = %d", rep.Unpriced)
	}
}

func TestCustomPartialZeroPriceIsApplied(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","message":{"id":"p","model":"claude-opus-5","usage":{"input_tokens":1000000,"output_tokens":1000000}}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), map[string]Price{"claude-opus-5": {In: 0, Out: 2}})
	if err != nil {
		t.Fatal(err)
	}
	m := rep.Models[0]
	if !m.Priced || !m.Custom || m.Cost < 1.999 || m.Cost > 2.001 {
		t.Fatalf("partial zero custom price = %+v", m)
	}
}

func TestAbsentCustomPriceFallsBackToPreset(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","message":{"id":"a","model":"claude-opus-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := newUsageScanner().Scan(filepath.Join(dir, "settings.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := rep.Models[0]
	if !m.Priced || m.Custom || m.Cost < 4.999 || m.Cost > 5.001 {
		t.Fatalf("preset fallback = %+v", m)
	}
}
