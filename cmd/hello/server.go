package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// serve runs h on ln until ctx is cancelled, then shuts down gracefully:
// it sets ready to false, stops accepting new connections and lets in-flight
// requests finish (up to shutdownTimeout).
//
// The handler and ready flag are passed in, not built here, so tests can use a
// handler that holds a request open during shutdown.
func serve(ctx context.Context, ln net.Listener, h http.Handler, ready *atomic.Bool,
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

// newHandler wires routes and middleware. Request ID runs first so the access log can include it.
func newHandler(logger *slog.Logger, ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleHello)
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(ready))

	return withRequestID(withAccessLog(logger, mux))
}

func handleHello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "hello from tiny-bank"})
}

// handleHealthz is the liveness check: "the process is alive". It never checks
// dependencies. If it fails, Kubernetes restarts the pod.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is the readiness check: "send me traffic". If it fails, Kubernetes
// stops routing to the pod but does not restart it.
func handleReadyz(ready *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // if the client has gone away there's nothing useful to do
}

// --- request ID ---

const (
	requestIDHeader = "X-Request-Id"
	maxRequestIDLen = 64
)

type requestIDKey struct{}

// withRequestID gives every request an ID: it reuses a caller's X-Request-Id if
// valid, otherwise generates one. The ID is stored in the context and echoed back.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// validRequestID accepts only short IDs of letters, digits, '-' and '_', so a caller
// can't inject newlines or huge values into our logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// --- access log ---

// withAccessLog writes one structured log line per request.
func withAccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)),
		)
	})
}

// statusRecorder remembers the status code a handler wrote, for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (e.g. to flush).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
