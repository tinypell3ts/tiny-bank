package kittest

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// kitService is the smallest service built the way services are: health
// routes on a Typhon router, behind kit.Handler.
func kitService() http.Handler {
	router := typhon.Router{}
	router.GET("/healthz", kit.Health)
	router.GET("/readyz", kit.Health)
	return kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router)
}

func TestAServiceOnKitConforms(t *testing.T) {
	Conformance(t, kitService)
}

// Each check must fail for a handler that breaks its rule, or it would pass
// a broken service and protect nothing.
func TestEachCheckCatchesItsRuleBeingBroken(t *testing.T) {
	tests := map[string]http.Handler{
		// liveness answers in plain text instead of JSON
		"liveness": overrideRoute(kitService(), "/healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "OK")
		}),
		// readiness is missing
		"readiness": overrideRoute(kitService(), "/readyz", http.NotFound),
		// filters copied into a service by hand, without the request ID
		"request ID": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del(kit.RequestIDHeader)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		}),
		// Typhon's ErrorFilter alone, which sends stack traces to the caller
		"unknown routes": typhon.HttpHandler(func() typhon.Service {
			router := typhon.Router{}
			router.GET("/healthz", func(req typhon.Request) typhon.Response {
				return typhon.Response{Error: terrors.NotFound("thing", "not here", nil)}
			})
			return router.Serve().Filter(typhon.ErrorFilter)
		}()),
		// metrics labelled by raw path
		"metrics": rawPathMetrics(kitService()),
	}
	for _, c := range checks {
		h, ok := tests[c.name]
		if !ok {
			t.Errorf("check %q has no broken handler to prove it fails", c.name)
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(h); err == nil {
				t.Errorf("check %q passed a handler that breaks its rule", c.name)
			} else {
				t.Logf("failed as it should: %v", err)
			}
		})
	}
}

// overrideRoute answers path with f and everything else with next.
func overrideRoute(next http.Handler, path string, f http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == path {
			f(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rawPathMetrics records every request under its raw path, the mistake the
// route-pattern rule exists to prevent, and serves those metrics.
func rawPathMetrics(next http.Handler) http.Handler {
	reg := obs.NewRegistry()
	m := obs.NewHTTPMetrics(reg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			obs.Handler(reg).ServeHTTP(w, r)
			return
		}
		done := m.Track()
		next.ServeHTTP(w, r)
		done(r.Method, strings.TrimSpace(r.URL.Path), http.StatusOK)
	})
}
