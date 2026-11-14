package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/services/account"
)

// newHandler builds the account service's routes on Typhon. kit.Handler adds
// the shared filters (request IDs, access logs, metrics, error handling) and
// GET /metrics.
func newHandler(logger *slog.Logger, ready *atomic.Bool, reg *prometheus.Registry,
	database pinger, accounts account.Accounts,
) http.Handler {
	router := typhon.Router{}
	router.GET("/healthz", kit.Health) // liveness never checks the database
	router.GET("/readyz", handleReadyz(ready, database))
	account.RegisterRoutes(&router, accounts)
	return kit.Handler(logger, reg, &router)
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
func handleReadyz(ready *atomic.Bool, database pinger) typhon.Service {
	return func(req typhon.Request) typhon.Response {
		if !ready.Load() {
			return req.ResponseWithCode(map[string]string{"status": "not ready"}, http.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(req.Context, readyPingTimeout)
		defer cancel()
		if err := database.Ping(ctx); err != nil {
			// The probe's caller is the kubelet, but keep the reason generic anyway:
			// internal errors never go to clients.
			return req.ResponseWithCode(map[string]string{"status": "database unavailable"}, http.StatusServiceUnavailable)
		}
		return req.Response(map[string]string{"status": "ok"})
	}
}
