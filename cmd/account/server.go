package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
	"github.com/tinypell3ts/tiny-bank/services/account"
)

// newHandler wires routes and middleware, outermost first:
//   - request ID, so everything after it can use the ID
//   - access log, one line per request
//   - metrics, directly around the mux so it can read the matched route
func newHandler(logger *slog.Logger, ready *atomic.Bool, reg *prometheus.Registry,
	database pinger, accounts account.Accounts,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", kit.Healthz) // liveness never checks the database
	mux.HandleFunc("GET /readyz", handleReadyz(ready, database))
	mux.Handle("GET /metrics", obs.Handler(reg)) // scraped by Prometheus
	account.RegisterRoutes(mux, accounts, logger)

	metrics := obs.NewHTTPMetrics(reg)
	return kit.RequestID(kit.AccessLog(logger, metrics.Middleware(mux)))
}

// pinger is the part of the database pool readiness needs. It's an interface so
// tests can stand in for a database that's up or down.
type pinger interface {
	Ping(ctx context.Context) error
}

// readyPingTimeout keeps a slow database from making the probe itself time out.
const readyPingTimeout = 2 * time.Second

// handleReadyz is the readiness check: "send me traffic". It fails while
// shutting down, or when the database doesn't answer. Kubernetes then stops
// routing to this pod but doesn't restart it, and routes again once it recovers.
func handleReadyz(ready *atomic.Bool, database pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			kit.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyPingTimeout)
		defer cancel()
		if err := database.Ping(ctx); err != nil {
			// The probe's caller is the kubelet, but keep the reason generic anyway:
			// internal errors never go to clients.
			kit.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
			return
		}
		kit.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
