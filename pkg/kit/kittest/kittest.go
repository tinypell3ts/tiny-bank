// Package kittest checks that a service follows the rules every tiny-bank
// service gets from pkg/kit, through the service's own handler:
//
//   - GET /healthz answers 200 {"status":"ok"} (liveness)
//   - GET /readyz answers 200 when the service is ready (readiness)
//   - every response carries an X-Request-Id: the caller's if it's valid,
//     otherwise a new one
//   - a path or method with no route is a 404 terror (not_found.*), and no
//     error ever carries a stack trace or source file path
//   - GET /metrics serves the RED metrics, labelled by route pattern, never by
//     raw path, and doesn't count its own scrapes
//
// A service runs it from a test, with a handler built the way main builds it:
//
//	func TestConformance(t *testing.T) {
//		kittest.Conformance(t, func() http.Handler { return newHandler(…) })
//	}
//
// Each check gets a fresh handler, so their metrics don't mix. The handler
// must be ready, with any dependencies (a database, say) faked as working.
// The service's own behaviour, such as when it's not ready, stays in its own
// tests.
package kittest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
)

// Conformance runs every check against handlers from newHandler, each as its
// own subtest.
func Conformance(t *testing.T, newHandler func() http.Handler) {
	t.Helper()
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(newHandler()); err != nil {
				t.Error(err)
			}
		})
	}
}

// A check returns an error saying how the handler broke one rule.
type check struct {
	name string
	run  func(h http.Handler) error
}

var checks = []check{
	{"liveness", checkLiveness},
	{"readiness", checkReadiness},
	{"request ID", checkRequestID},
	{"unknown routes", checkUnknownRoutes},
	{"metrics", checkMetrics},
}

// unroutedPath is a path no service will ever have.
const unroutedPath = "/kittest-no-such-route"

func checkLiveness(h http.Handler) error {
	rec := serve(h, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		return fmt.Errorf(`GET /healthz = %d %s, want 200 {"status":"ok"}`, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		return fmt.Errorf("GET /healthz Content-Type = %q, want application/json", ct)
	}
	return nil
}

func checkReadiness(h http.Handler) error {
	if rec := serve(h, http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
		return fmt.Errorf("GET /readyz = %d %s, want 200 from a ready service", rec.Code, rec.Body)
	}
	return nil
}

func checkRequestID(h http.Handler) error {
	var errs []error
	// The caller's ID is reused, on success and on errors, so one ID follows
	// a request through every service's logs.
	for _, path := range []string{"/healthz", unroutedPath} {
		rec := serve(h, http.MethodGet, path, http.Header{kit.RequestIDHeader: {"kittest-123"}})
		if got := rec.Header().Get(kit.RequestIDHeader); got != "kittest-123" {
			errs = append(errs, fmt.Errorf("GET %s with a valid %s: got %q back, want it reused", path, kit.RequestIDHeader, got))
		}
	}
	// A missing or unsafe ID is replaced with a new one.
	for _, sent := range []string{"", "bad\nid", strings.Repeat("a", 65)} {
		header := http.Header{}
		if sent != "" {
			header.Set(kit.RequestIDHeader, sent)
		}
		got := serve(h, http.MethodGet, "/healthz", header).Header().Get(kit.RequestIDHeader)
		if got == "" || got == sent || strings.ContainsAny(got, "\r\n") {
			errs = append(errs, fmt.Errorf("GET /healthz with %s %q: got %q back, want a new safe ID", kit.RequestIDHeader, sent, got))
		}
	}
	return errors.Join(errs...)
}

func checkUnknownRoutes(h http.Handler) error {
	var errs []error
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, unroutedPath},
		{http.MethodDelete, "/healthz"}, // Typhon's router has no 405: a wrong method is a 404
	} {
		rec := serve(h, req.method, req.path, nil)
		var body struct{ Code string }
		if rec.Code != http.StatusNotFound || json.Unmarshal(rec.Body.Bytes(), &body) != nil || !strings.HasPrefix(body.Code, "not_found") {
			errs = append(errs, fmt.Errorf("%s %s = %d %s, want a 404 not_found terror", req.method, req.path, rec.Code, rec.Body))
		}
		if leaksInternals(rec.Body.String()) {
			errs = append(errs, fmt.Errorf("%s %s leaks a stack trace or file path: %s", req.method, req.path, rec.Body))
		}
	}
	return errors.Join(errs...)
}

// leaksInternals reports whether an error body carries the server's own
// details, which Typhon sends unless kit.Handler strips them.
func leaksInternals(body string) bool {
	for _, s := range []string{"stack", "filename", "message_chain", ".go:"} {
		if strings.Contains(body, s) {
			return true
		}
	}
	return false
}

func checkMetrics(h http.Handler) error {
	serve(h, http.MethodGet, "/healthz", nil)
	serve(h, http.MethodGet, unroutedPath, nil)
	serve(h, http.MethodGet, "/metrics", nil) // a scrape, which must not count as traffic

	rec := serve(h, http.MethodGet, "/metrics", nil)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /metrics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	var errs []error
	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/healthz"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="unmatched"} 1`,
		"http_server_active_requests 0",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			errs = append(errs, fmt.Errorf("GET /metrics is missing %s", want))
		}
	}
	if strings.Contains(body, unroutedPath) {
		errs = append(errs, errors.New("GET /metrics labels a raw path: routes must be patterns, or every path makes a new time series"))
	}
	if strings.Contains(body, `http_route="/metrics"`) {
		errs = append(errs, errors.New("GET /metrics counts its own scrapes as traffic"))
	}
	return errors.Join(errs...)
}

func serve(h http.Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
