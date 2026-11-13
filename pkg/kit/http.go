package kit

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// Healthz is the liveness check: "the process is alive". It never checks
// dependencies such as the database. If it fails, Kubernetes restarts the pod,
// and restarting every pod because a dependency blinked makes things worse.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // if the client has gone away there's nothing useful to do
}

// --- request ID ---

// RequestIDHeader is read from callers and echoed back.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// RequestID gives every request an ID: it reuses a caller's X-Request-Id if
// valid, otherwise generates one. The ID is stored in the context and echoed
// back, so logs (and, later, downstream calls) can be joined to one request.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDFrom returns the request ID that RequestID stored in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestIDPattern is 1 to 64 ASCII letters, digits, '-' or '_'. Go's $ matches
// only the very end of the string, so a trailing newline is rejected too.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validRequestID accepts only short IDs of letters, digits, '-' and '_', so a caller
// can't inject newlines or huge values into our logs.
func validRequestID(id string) bool {
	return requestIDPattern.MatchString(id)
}

// --- access log ---

// AccessLog writes one structured log line per request. It reads the request
// ID, so it must run inside RequestID.
func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := obs.NewStatusRecorder(w)
		next.ServeHTTP(rec, r)

		logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("request_id", RequestIDFrom(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.Status()),
			slog.Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)),
		)
	})
}
