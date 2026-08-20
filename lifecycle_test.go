package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHealthIdentityIncludesPIDAndNonce(t *testing.T) {
	p := NewProxy(DefaultConfig(), log.New(io.Discard, "", 0))
	r := httptest.NewRequest(http.MethodGet, "/__ccproxy/health", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var h healthIdentity
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if !h.OK || h.PID != os.Getpid() || h.Nonce == "" || h.Nonce != p.nonce {
		t.Fatalf("health identity = %+v", h)
	}
}

func TestShutdownEndpointRequiresNonceAndRejectsCrossOrigin(t *testing.T) {
	p := NewProxy(DefaultConfig(), log.New(io.Discard, "", 0))
	for _, tc := range []struct {
		name   string
		nonce  string
		origin string
	}{
		{name: "missing nonce"},
		{name: "wrong nonce", nonce: "wrong"},
		{name: "cross origin", nonce: p.nonce, origin: "https://example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/__ccproxy/shutdown", nil)
			if tc.nonce != "" {
				r.Header.Set(shutdownNonceHeader, tc.nonce)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
			}
			select {
			case <-p.shutdownCh:
				t.Fatal("shutdown triggered by rejected request")
			default:
			}
		})
	}
}

func netListenLoopback() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

func TestShutdownEndpointTriggersLifecycleCleanup(t *testing.T) {
	p := NewProxy(DefaultConfig(), log.New(io.Discard, "", 0))
	p.meter.Add("provider", "Provider", "model", 1, 2, 3, 4)
	flushed := make(chan struct{}, 1)
	p.meter.write = func(string, []byte, os.FileMode) error {
		flushed <- struct{}{}
		return nil
	}

	errCh := make(chan error, 1)
	ln, err := netListenLoopback()
	if err != nil {
		t.Fatal(err)
	}
	srv := p.newServer()
	p.mu.Lock()
	p.srv = srv
	p.mu.Unlock()
	go func() { errCh <- p.serve(srv, ln) }()

	cleanupCalled := false
	cleanup := func() error { cleanupCalled = true; return nil }
	done := make(chan error, 1)
	go func() {
		<-p.shutdownCh
		done <- shutdownDaemonWithCleanup(p, errCh, cleanup)
	}()

	r := httptest.NewRequest(http.MethodPost, "/__ccproxy/shutdown", nil)
	r.Header.Set(shutdownNonceHeader, p.nonce)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !cleanupCalled {
		t.Fatal("lifecycle cleanup was not called")
	}
	select {
	case <-flushed:
	default:
		t.Fatal("final meter flush was not called")
	}
}

func TestStopLateMatchingDaemonOnlyStopsAuthenticatedIdentity(t *testing.T) {
	oldMatch, oldStop := identityMatchesForLateStop, stopDaemonForLateStop
	defer func() {
		identityMatchesForLateStop, stopDaemonForLateStop = oldMatch, oldStop
	}()

	stopped := false
	identityMatchesForLateStop = func(int, time.Duration) error { return errors.New("unrelated listener") }
	stopDaemonForLateStop = func() error { stopped = true; return nil }
	if err := stopLateMatchingDaemon(15722, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if stopped {
		t.Fatal("stopped a listener without matching authenticated identity")
	}

	identityMatchesForLateStop = func(port int, _ time.Duration) error {
		if port != 15722 {
			t.Fatalf("port = %d", port)
		}
		return nil
	}
	if err := stopLateMatchingDaemon(15722, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("matching late daemon was not stopped")
	}
}

func TestStopDaemonRequiresMatchingHealthIdentity(t *testing.T) {
	oldRead, oldClear, oldHealth := readStatusForStop, clearStatusForStop, readHealthForStop
	oldShutdown, oldWait := requestShutdownForStop, waitPortFreeForStop
	defer func() {
		readStatusForStop, clearStatusForStop, readHealthForStop = oldRead, oldClear, oldHealth
		requestShutdownForStop, waitPortFreeForStop = oldShutdown, oldWait
	}()
	cleared, requested := false, false
	readStatusForStop = func() (*Status, bool) {
		return &Status{PID: 4242, Port: 1234, Nonce: "expected"}, true
	}
	clearStatusForStop = func() error { cleared = true; return nil }
	requestShutdownForStop = func(int, string, time.Duration) error { requested = true; return nil }
	waitPortFreeForStop = func(int, time.Duration) bool { return true }
	readHealthForStop = func(int, time.Duration) (healthIdentity, error) {
		return healthIdentity{OK: true, PID: 4242, Nonce: "other"}, nil
	}
	if err := StopDaemon(); err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("mismatch error = %v", err)
	}
	if requested || cleared {
		t.Fatalf("mismatch requested=%v cleared=%v", requested, cleared)
	}

	readHealthForStop = func(int, time.Duration) (healthIdentity, error) {
		return healthIdentity{OK: true, PID: 4242, Nonce: "expected"}, nil
	}
	shutdownErr := errors.New("denied")
	requestShutdownForStop = func(int, string, time.Duration) error { return shutdownErr }
	if err := StopDaemon(); !errors.Is(err, shutdownErr) {
		t.Fatalf("shutdown error = %v", err)
	}
	if cleared {
		t.Fatal("status cleared after failed shutdown request")
	}

	requestShutdownForStop = func(port int, nonce string, _ time.Duration) error {
		requested = port == 1234 && nonce == "expected"
		return nil
	}
	if err := StopDaemon(); err != nil {
		t.Fatal(err)
	}
	if !requested || !cleared {
		t.Fatalf("success requested=%v cleared=%v", requested, cleared)
	}
}
