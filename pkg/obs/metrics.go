// Package obs holds the observability building blocks shared by every service.
// For now that is Prometheus metrics for HTTP servers. Logging and tracing join
// it in later phases, when a second service needs them.
package obs

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry returns a metrics registry with Go runtime and process metrics
// (goroutines, memory, GC, CPU, open files) already registered.
//
// Each service owns its registry, not the global default one, so tests can
// create a fresh registry and nothing registers metrics behind our back.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// Handler serves the registry's metrics in Prometheus text format, for /metrics.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

// HTTPMetrics records RED metrics for an HTTP server from ONE histogram,
// http_server_request_duration_seconds{http_request_method, http_route, http_response_status_code}:
//   - Rate:     the histogram's _count series
//   - Errors:   _count filtered by status code (http_response_status_code=~"5..")
//   - Duration: the histogram's buckets (p50, p95, p99), including for errors
//
// plus http_server_active_requests, to show load right now. Names and labels
// follow the OpenTelemetry HTTP semantic conventions, so dashboards keep working
// when tracing arrives.
type HTTPMetrics struct {
	duration *prometheus.HistogramVec
	active   prometheus.Gauge
}

// NewHTTPMetrics creates the HTTP metrics and registers them on reg.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_server_request_duration_seconds",
			Help: "Time to handle an HTTP request, by method, route and status code.",
			// 1ms to 10s: fine-grained where a healthy service lives, coarse in the tail.
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"http_request_method", "http_route", "http_response_status_code"}),
		active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_server_active_requests",
			Help: "HTTP requests currently being handled.",
		}),
	}
	reg.MustRegister(m.duration, m.active)
	return m
}

// Track starts recording one request and returns a function to call when it
// ends, with its method, route pattern and status. Transports that don't use
// http.ServeMux (Typhon, in pkg/kit) record through it; Middleware uses it too.
//
// route must be the pattern that matched, like "/accounts/:id", never the raw
// URL path. Raw paths would create one time series per account ID and
// overload Prometheus ("cardinality explosion"). An empty route means the
// request matched nothing and is labelled "unmatched".
func (m *HTTPMetrics) Track() (done func(method, route string, status int)) {
	m.active.Inc()
	start := time.Now()
	return func(method, route string, status int) {
		m.active.Dec()
		m.duration.WithLabelValues(methodOf(method), routeOf(route), strconv.Itoa(status)).
			Observe(time.Since(start).Seconds())
	}
}

// Middleware records metrics for every request handled by next, which must
// be (or wrap) an http.ServeMux: the route label is the mux pattern that matched.
func (m *HTTPMetrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done := m.Track()
		rec := NewStatusRecorder(w)
		next.ServeHTTP(rec, r)
		// The mux sets r.Pattern on this same request while routing it.
		done(r.Method, r.Pattern, rec.Status())
	})
}

// routeOf turns a mux pattern like "GET /accounts/{id}" into "/accounts/{id}".
// The method is already its own label.
func routeOf(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}

// knownMethods are the standard HTTP methods. Anything else a caller sends is
// recorded as "_OTHER" (the OpenTelemetry convention), because the method comes
// straight from the client: without this, a caller could send CUSTOM1, CUSTOM2, …
// and create a new time series, using more memory, for every one.
var knownMethods = map[string]bool{
	http.MethodConnect: true, http.MethodDelete: true, http.MethodGet: true,
	http.MethodHead: true, http.MethodOptions: true, http.MethodPatch: true,
	http.MethodPost: true, http.MethodPut: true, http.MethodTrace: true,
}

func methodOf(method string) string {
	if knownMethods[method] {
		return method
	}
	return "_OTHER"
}

// StatusRecorder wraps an http.ResponseWriter and remembers the final status
// code actually sent to the client, for metrics and access logs.
//
// "Final" matters: a handler may send informational 1xx responses (like
// 103 Early Hints) before the real one, and net/http ignores any WriteHeader
// after the first final status. If the handler never calls WriteHeader, the
// status is the implicit 200 that net/http sends.
type StatusRecorder struct {
	http.ResponseWriter
	status int // 0 until a final status is sent
}

// NewStatusRecorder wraps w.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w}
}

// Status returns the final status code sent, or 200 if nothing was written yet.
func (r *StatusRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// WriteHeader records the first final (non-1xx) status and passes the call on.
func (r *StatusRecorder) WriteHeader(code int) {
	if r.status == 0 && code >= http.StatusOK {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write records the implicit 200 when the body starts without a WriteHeader.
func (r *StatusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer (e.g. to flush).
func (r *StatusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
