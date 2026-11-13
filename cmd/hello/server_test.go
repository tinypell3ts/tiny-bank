package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
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
		{"wrong method", http.MethodPost, "/healthz", true, http.StatusNotFound, ""}, // Typhon's router answers 404, not 405
		{"unknown path", http.MethodGet, "/nope", true, http.StatusNotFound, ""},
		{"metrics", http.MethodGet, "/metrics", true, http.StatusOK, "go_goroutines"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool
			ready.Store(tt.ready)
			h := newHandler(slog.New(slog.DiscardHandler), &ready, obs.NewRegistry())

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

// TestMetricsCountRequests checks the middleware is wired in: requests through
// the real handler show up on /metrics, labelled by route, not raw path.
func TestMetricsCountRequests(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	h := newHandler(slog.New(slog.DiscardHandler), &ready, obs.NewRegistry())

	for _, path := range []string{"/", "/", "/healthz", "/does-not-exist"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/"} 2`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/healthz"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="unmatched"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
	if strings.Contains(body, "does-not-exist") {
		t.Error("/metrics must not contain raw request paths")
	}
}
