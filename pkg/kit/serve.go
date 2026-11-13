package kit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Serve runs h on ln until ctx is cancelled, then shuts down gracefully:
// it sets ready to false, stops accepting new connections and lets in-flight
// requests finish (up to shutdownTimeout).
//
// The handler and ready flag are passed in, not built here, so tests can use a
// handler that holds a request open during shutdown.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, ready *atomic.Bool,
	logger *slog.Logger, shutdownTimeout time.Duration,
) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second, // guards against slow-header (Slowloris) clients
	}

	// The listener is already bound, so we can accept traffic as soon as Serve starts.
	ready.Store(true)
	errCh := make(chan error, 1) // buffered so the goroutine can always exit
	go func() { errCh <- srv.Serve(ln) }()
	logger.Info("server started", "addr", ln.Addr().String())

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", shutdownTimeout.String())
	ready.Store(false) // readiness probe now fails, so Kubernetes stops routing new traffic here

	// ctx is already cancelled; shutdown needs its own deadline.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	logger.Info("server stopped")
	return nil
}
