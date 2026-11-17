package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// newReadyHandler is hello's handler as main builds it, ready to serve.
func newReadyHandler() http.Handler {
	router := typhon.Router{}
	routes(&router)
	var ready atomic.Bool
	ready.Store(true)
	return kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, &ready, nil)
}

// TestConformance checks hello follows every rule services get from pkg/kit:
// health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	kittest.ConformanceOf(t, routes)
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
