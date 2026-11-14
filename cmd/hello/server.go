package main

import (
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
)

// newHandler builds hello's routes on Typhon. kit.Handler adds the shared
// filters (request IDs, access logs, metrics, error handling) and /metrics.
func newHandler(logger *slog.Logger, ready *atomic.Bool, reg *prometheus.Registry) http.Handler {
	router := typhon.Router{}
	router.GET("/", handleHello)
	router.GET("/healthz", kit.Health)
	router.GET("/readyz", handleReadyz(ready))
	return kit.Handler(logger, reg, &router)
}

func handleHello(req typhon.Request) typhon.Response {
	return req.Response(map[string]string{"message": "hello from tiny-bank"})
}

// handleReadyz is the readiness check: "send me traffic". hello has no
// dependencies, so it's ready whenever it's serving. If it fails, Kubernetes
// stops routing to the pod but does not restart it.
func handleReadyz(ready *atomic.Bool) typhon.Service {
	return func(req typhon.Request) typhon.Response {
		if !ready.Load() {
			return req.ResponseWithCode(map[string]string{"status": "not ready"}, http.StatusServiceUnavailable)
		}
		return req.Response(map[string]string{"status": "ok"})
	}
}
