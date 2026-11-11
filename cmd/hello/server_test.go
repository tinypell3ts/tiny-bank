package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandlerRoutes(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		ready      bool
		wantStatus int
		wantBody   string // substring; empty means don't check
	}{
		{"hello", http.MethodGet, "/", true, http.StatusOK, `"message":"hello from tiny-bank"`},
		{"healthz", http.MethodGet, "/healthz", true, http.StatusOK, `"status":"ok"`},
		{"healthz ok even when not ready", http.MethodGet, "/healthz", false, http.StatusOK, `"status":"ok"`},
		{"readyz when ready", http.MethodGet, "/readyz", true, http.StatusOK, `"status":"ok"`},
		{"readyz when not ready", http.MethodGet, "/readyz", false, http.StatusServiceUnavailable, `"status":"not ready"`},
		{"wrong method", http.MethodPost, "/healthz", true, http.StatusMethodNotAllowed, ""},
		{"unknown path", http.MethodGet, "/nope", true, http.StatusNotFound, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool
			ready.Store(tt.ready)
			h := newHandler(slog.New(slog.DiscardHandler), &ready)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestRequestIDAndAccessLog(t *testing.T) {
	tests := []struct {
		name     string
		incoming string // X-Request-Id sent by the caller
		wantSame bool   // expect the incoming ID to be reused
	}{
		{name: "generated when missing", incoming: ""},
		{name: "reused when valid", incoming: "abc-123_XYZ", wantSame: true},
		{name: "replaced when it contains a newline", incoming: "bad\nid"},
		{name: "replaced when too long", incoming: strings.Repeat("a", maxRequestIDLen+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			var ready atomic.Bool
			ready.Store(true)
			h := newHandler(slog.New(slog.NewJSONHandler(&logs, nil)), &ready)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			if tt.incoming != "" {
				req.Header.Set(requestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(requestIDHeader)
			if !validRequestID(got) {
				t.Fatalf("response request ID %q is not valid", got)
			}
			if tt.wantSame && got != tt.incoming {
				t.Errorf("request ID = %q, want caller's %q", got, tt.incoming)
			}
			if !tt.wantSame && got == tt.incoming {
				t.Errorf("request ID %q should have been replaced", got)
			}

			// The access log line must carry the same ID, so logs can be joined to a request.
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("log is not one JSON line: %v\n%s", err, logs.String())
			}
			if entry["request_id"] != got {
				t.Errorf("logged request_id = %v, want %q", entry["request_id"], got)
			}
			if entry["status"] != float64(http.StatusOK) || entry["path"] != "/healthz" || entry["method"] != "GET" {
				t.Errorf("log entry missing request details: %v", entry)
			}
		})
	}
}

func TestServeShutsDownIdleServer(t *testing.T) {
	ln := listen(t)
	url := "http://" + ln.Addr().String() + "/readyz"

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, newHandler(slog.New(slog.DiscardHandler), &ready), &ready, 5*time.Second)

	// While running, the server is ready.
	if status, err := doGet(t.Context(), url); err != nil || status != http.StatusOK {
		t.Fatalf("readyz = %d, %v; want %d", status, err, http.StatusOK)
	}

	// Cancelling the context (what SIGTERM does in main) must make serve return cleanly.
	cancel()
	if err := waitFor(t, serveErr); err != nil {
		t.Fatalf("serve returned error: %v", err)
	}

	// After shutdown the port is closed.
	if _, err := doGet(t.Context(), url); err == nil {
		t.Fatal("expected request to fail after shutdown")
	}
}

func TestServeWaitsForInFlightRequest(t *testing.T) {
	ln := listen(t)
	h, started, release := blockingHandler()

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, h, &ready, 5*time.Second)

	// Start a request that stays open until we release it.
	respStatus := make(chan int, 1)
	go func() {
		status, _ := doGet(context.WithoutCancel(t.Context()), "http://"+ln.Addr().String()+"/")
		respStatus <- status // 0 if the request failed
	}()
	<-started

	// Shut down while that request is still running.
	cancel()

	// serve must wait for the request instead of returning straight away.
	select {
	case err := <-serveErr:
		t.Fatalf("serve returned (%v) while a request was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	if ready.Load() {
		t.Error("ready should be false once shutdown has started")
	}

	// Let the request finish: the client gets its response and serve returns cleanly.
	close(release)
	if status := <-respStatus; status != http.StatusOK {
		t.Errorf("in-flight request status = %d, want %d", status, http.StatusOK)
	}
	if err := waitFor(t, serveErr); err != nil {
		t.Fatalf("serve returned error: %v", err)
	}
}

func TestServeGivesUpAfterShutdownTimeout(t *testing.T) {
	ln := listen(t)
	h, started, release := blockingHandler()
	t.Cleanup(func() { close(release) }) // unblock the handler when the test ends

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, h, &ready, 50*time.Millisecond)

	go func() { _, _ = doGet(context.WithoutCancel(t.Context()), "http://"+ln.Addr().String()+"/") }()
	<-started
	cancel()

	// The request never finishes, so shutdown must stop waiting and report it.
	err := waitFor(t, serveErr)
	if err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("err = %v, want a shutdown timeout error", err)
	}
}

// --- helpers ---

// listen opens a listener on a free local port (port 0 = let the OS pick).
func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startServe runs serve in a goroutine and returns a channel that receives its result.
func startServe(ctx context.Context, ln net.Listener, h http.Handler, ready *atomic.Bool, timeout time.Duration) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, ln, h, ready, slog.New(slog.DiscardHandler), timeout) }()
	return errCh
}

// waitFor returns serve's result, failing the test if it takes too long.
func waitFor(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
		return nil
	}
}

// blockingHandler signals on started when a request arrives, then holds it open
// until release is closed.
func blockingHandler() (h http.Handler, started <-chan struct{}, release chan struct{}) {
	s := make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	h = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(s) })
		<-release
		w.WriteHeader(http.StatusOK)
	})
	return h, s, release
}

// doGet makes a GET request and returns the status code. It doesn't call t.Fatal,
// so it's safe to use from other goroutines.
func doGet(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
