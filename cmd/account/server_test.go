package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// fakeDB stands in for the database pool: Ping returns err.
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

// slowDB never answers, to check the readiness probe doesn't wait forever.
type slowDB struct{}

func (slowDB) Ping(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestHandlerRoutes(t *testing.T) {
	down := fakeDB{err: errors.New("connection refused: secret detail")}
	tests := []struct {
		name       string
		path       string
		ready      bool
		db         pinger
		wantStatus int
		wantBody   string // substring; empty means don't check
	}{
		{"healthz ok", "/healthz", true, fakeDB{}, http.StatusOK, `"status":"ok"`},
		{"healthz ignores the database", "/healthz", true, down, http.StatusOK, `"status":"ok"`},
		{"readyz ok", "/readyz", true, fakeDB{}, http.StatusOK, `"status":"ok"`},
		{"readyz while shutting down", "/readyz", false, fakeDB{}, http.StatusServiceUnavailable, `"status":"not ready"`},
		{"readyz when the database is down", "/readyz", true, down, http.StatusServiceUnavailable, `"status":"database unavailable"`},
		{"metrics", "/metrics", true, fakeDB{}, http.StatusOK, "go_goroutines"},
		{"unknown path", "/nope", true, fakeDB{}, http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ready atomic.Bool
			ready.Store(tt.ready)
			h := newHandler(slog.New(slog.DiscardHandler), &ready, obs.NewRegistry(), tt.db)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
			if strings.Contains(rec.Body.String(), "secret detail") {
				t.Errorf("body leaks the internal error: %q", rec.Body.String())
			}
		})
	}
}

func TestReadyzGivesUpOnASlowDatabase(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	h := newHandler(slog.New(slog.DiscardHandler), &ready, obs.NewRegistry(), slowDB{})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if took := time.Since(start); took > readyPingTimeout+time.Second {
		t.Errorf("readyz took %v, want it to give up after about %v", took, readyPingTimeout)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	err := run(t.Context(), func(string) string { return "" }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("err = %v, want the missing DATABASE_URL error", err)
	}
}

func TestRunGivesUpWhenTheDatabaseNeverAnswers(t *testing.T) {
	// Nothing listens on port 1, so every attempt fails. The caller's
	// deadline (here 3s instead of the real 2 minutes) must stop the retries.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	env := map[string]string{"DATABASE_URL": "postgres://app:s3cret@127.0.0.1:1/app?sslmode=disable"} //nolint:gosec // fake credentials, nothing listens there

	var logs strings.Builder
	err := run(ctx, func(k string) string { return env[k] }, &logs)
	if err == nil || !strings.Contains(err.Error(), "database not reachable before startup timeout") {
		t.Fatalf("err = %v, want the startup timeout error", err)
	}
	if !strings.Contains(logs.String(), "database not ready, retrying") {
		t.Errorf("expected retry log lines, got:\n%s", logs.String())
	}
	if strings.Contains(err.Error()+logs.String(), "s3cret") {
		t.Error("error or logs leak the database password")
	}
}
