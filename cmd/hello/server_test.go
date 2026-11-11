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

func TestServeShutsDownGracefully(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0") // port 0 = pick any free port
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/readyz"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, ln, slog.New(slog.DiscardHandler), 5*time.Second) }()

	// While running, the server is ready.
	if status := get(t, url); status != http.StatusOK {
		t.Fatalf("readyz status = %d, want %d", status, http.StatusOK)
	}

	// Cancelling the context (what SIGTERM does in main) must make serve return cleanly.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}

	// After shutdown the port is closed.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected request to fail after shutdown")
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
