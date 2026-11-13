package main

import (
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// newHandler wires routes and middleware, outermost first:
//   - request ID, so everything after it can use the ID
//   - access log, one line per request
//   - metrics, directly around the mux so it can read the matched route
func newHandler(logger *slog.Logger, ready *atomic.Bool, reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleHello)
	mux.HandleFunc("GET /healthz", kit.Healthz)
	mux.HandleFunc("GET /readyz", handleReadyz(ready))
	mux.Handle("GET /metrics", obs.Handler(reg)) // scraped by Prometheus

	metrics := obs.NewHTTPMetrics(reg)
	return kit.RequestID(kit.AccessLog(logger, metrics.Middleware(mux)))
}

func handleHello(w http.ResponseWriter, _ *http.Request) {
	kit.WriteJSON(w, http.StatusOK, map[string]string{"message": "hello from tiny-bank"})
}

// handleReadyz is the readiness check: "send me traffic". hello has no
// dependencies, so it's ready whenever it's serving. If it fails, Kubernetes
// stops routing to the pod but does not restart it.
func handleReadyz(ready *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			kit.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		kit.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
