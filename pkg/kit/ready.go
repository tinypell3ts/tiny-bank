package kit

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/monzo/typhon"
)

// readyCheckTimeout keeps a slow dependency from making the probe itself time
// out, which Kubernetes would count as a failure anyway, but later.
const readyCheckTimeout = 2 * time.Second

// readyz is the readiness check, served at /readyz by Handler: "send me traffic". It
// fails while the service is starting or shutting down (ready is false), and
// when check, the dependency the service can't work without (its database's
// Ping, say), fails or takes longer than 2 seconds. A service with no such
// dependency passes nil.
//
// When it fails, Kubernetes stops routing to the pod but doesn't restart it,
// and routes to it again once it passes. The body never says why a check
// failed: internal errors don't go to callers, even the kubelet.
func readyz(ready *atomic.Bool, check func(context.Context) error) typhon.Service {
	return func(req typhon.Request) typhon.Response {
		if !ready.Load() {
			return req.ResponseWithCode(map[string]string{"status": "not ready"}, http.StatusServiceUnavailable)
		}
		if check != nil {
			ctx, cancel := context.WithTimeout(req.Context, readyCheckTimeout)
			defer cancel()
			if err := check(ctx); err != nil {
				return req.ResponseWithCode(map[string]string{"status": "dependency unavailable"}, http.StatusServiceUnavailable)
			}
		}
		return req.Response(map[string]string{"status": "ok"})
	}
}
