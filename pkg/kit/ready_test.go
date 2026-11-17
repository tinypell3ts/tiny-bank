package kit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

func serveReadyz(t *testing.T, h typhon.Service) *httptest.ResponseRecorder {
	t.Helper()
	router := typhon.Router{}
	router.GET("/readyz", h)
	rec := httptest.NewRecorder()
	typhon.HttpHandler(router.Serve()).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	return rec
}

func TestReadyz(t *testing.T) {
	up := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("connection refused: secret detail") }
	tests := []struct {
		name       string
		ready      bool
		check      func(context.Context) error
		wantStatus int
		wantBody   string
	}{
		{"ready, no dependency", true, nil, http.StatusOK, `"status":"ok"`},
		{"ready, dependency up", true, up, http.StatusOK, `"status":"ok"`},
		{"ready, dependency down", true, down, http.StatusServiceUnavailable, `"status":"dependency unavailable"`},
		{"shutting down", false, up, http.StatusServiceUnavailable, `"status":"not ready"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool
			ready.Store(tt.ready)
			rec := serveReadyz(t, readyz(&ready, tt.check))
			if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("readyz = %d %s, want %d with %s", rec.Code, rec.Body, tt.wantStatus, tt.wantBody)
			}
			if strings.Contains(rec.Body.String(), "secret detail") {
				t.Errorf("body leaks the dependency's error: %s", rec.Body)
			}
		})
	}
}

func TestReadyzGivesUpOnASlowDependency(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	never := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

	start := time.Now()
	rec := serveReadyz(t, readyz(&ready, never))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if took := time.Since(start); took > readyCheckTimeout+time.Second {
		t.Errorf("readyz took %v, want it to give up after about %v", took, readyCheckTimeout)
	}
}

// readyNow is a ready flag that's already set, for tests that need a
// handler but aren't about readiness.
func readyNow() *atomic.Bool {
	var ready atomic.Bool
	ready.Store(true)
	return &ready
}

// TestHandlerServesTheProbes checks Handler adds /healthz and /readyz, and
// that only readiness depends on the service's dependency.
func TestHandlerServesTheProbes(t *testing.T) {
	down := func(context.Context) error { return errors.New("connection refused") }
	h := Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &typhon.Router{}, readyNow(), down)
	for path, want := range map[string]int{"/readyz": http.StatusServiceUnavailable, "/healthz": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s with the dependency down = %d, want %d", path, rec.Code, want)
		}
	}
}
