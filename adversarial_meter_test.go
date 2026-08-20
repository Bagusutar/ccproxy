package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func Test403MeterExactlyOnceForMatchingAndNonmatching(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want []byte
	}{
		{"matching", []byte(`{"error":{"message":"no access to model m"},"usage":{"input_tokens":8,"output_tokens":2}}`), nil},
		{"nonmatching", []byte(`{"error":{"message":"other"},"usage":{"input_tokens":9,"output_tokens":3}}`), []byte(`{"error":{"message":"other"},"usage":{"input_tokens":9,"output_tokens":3}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meter := newUsageMeter()
			p := &Proxy{meter: meter, logf: log.New(io.Discard, "", 0)}
			tg := &target{id: "p", name: "p", model: "m"}
			req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages", nil)
			req = req.WithContext(context.WithValue(req.Context(), ctxTarget, tg))
			resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(tc.body)), ContentLength: int64(len(tc.body)), Request: req}
			if err := p.onResponse(resp); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != nil && !bytes.Equal(got, tc.want) {
				t.Fatalf("body changed: %q", got)
			}
			rows := meter.snapshot().Rows
			if len(rows) != 1 || rows[0].Reqs != 1 {
				t.Fatalf("rows=%+v", rows)
			}
			if tc.name == "matching" && (rows[0].In != 8 || rows[0].Out != 2) {
				t.Fatalf("usage=%+v", rows[0])
			}
			if tc.name == "nonmatching" && (rows[0].In != 9 || rows[0].Out != 3) {
				t.Fatalf("usage=%+v", rows[0])
			}
		})
	}
}
