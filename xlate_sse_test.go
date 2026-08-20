package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestSSEAssemblerSpec(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"event-only", "event: e\n\n", ""},
		{"empty-data", "event: e\ndata:\n\n", "e:"},
		{"multi-data", "event: e\ndata: a\ndata: b\n\n", "e:a\nb"},
		{"optional-space", "event:  e\ndata:  x\n\n", " e: x"},
		{"crlf", "event: e\r\ndata: x\r\n\r\n", "e:x"},
		{"eof-dispatch", "event: e\ndata: x", "e:x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			s := newSSEXlate(io.NopCloser(strings.NewReader(tc.in)))
			s.handle = func(e string, d []byte) { got = append(got, e+":"+string(d)) }
			if _, err := io.ReadAll(&s); err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestSSEAssemblerOversizeRecovery(t *testing.T) {
	for _, field := range []string{"event: ", ":", "unknown: ", "data: "} {
		t.Run(field, func(t *testing.T) {
			in := "event: bad\ndata: x\n" + field + string(bytes.Repeat([]byte{'z'}, sseEventCap+1)) + "\n\n" +
				"event: good\ndata: ok\n\n"
			var got []string
			s := newSSEXlate(io.NopCloser(strings.NewReader(in)))
			s.handle = func(e string, d []byte) { got = append(got, e+":"+string(d)) }
			if _, err := io.ReadAll(&s); err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != "good:ok" {
				t.Fatalf("got %q", got)
			}
		})
	}
}
