package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

func newReadyHandler() http.Handler {
	var ready atomic.Bool
	ready.Store(true)
	return newHandler(slog.New(slog.DiscardHandler), &ready, obs.NewRegistry())
}

// TestConformance checks hello follows every rule services get from pkg/kit:
// health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	kittest.Conformance(t, newReadyHandler)
}

func TestHello(t *testing.T) {
	h := newReadyHandler()
	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"message":"hello from tiny-bank"`) {
			t.Errorf("GET / = %d %s", rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	want := `http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/"} 2`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("/metrics missing %s", want)
	}
}

func TestReadyzWhenNotReady(t *testing.T) {
	var ready atomic.Bool // false: shutting down
	h := newHandler(slog.New(slog.DiscardHandler), &ready, obs.NewRegistry())
	for path, want := range map[string]int{"/readyz": http.StatusServiceUnavailable, "/healthz": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s while shutting down = %d, want %d", path, rec.Code, want)
		}
	}
}
