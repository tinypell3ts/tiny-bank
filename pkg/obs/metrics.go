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

// Middleware records metrics for every request handled by next.
//
// next must be (or wrap) an http.ServeMux: the route label is the mux pattern
// that matched, like "/accounts/{id}", never the raw URL path. Raw paths
// would create one time series per account ID and overload Prometheus
// ("cardinality explosion"). Requests that match no route are labelled "unmatched".
func (m *HTTPMetrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.active.Inc()
		defer m.active.Dec()

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// The mux sets r.Pattern on this same request while routing it.
		m.duration.WithLabelValues(r.Method, routeOf(r.Pattern), strconv.Itoa(rec.status)).
			Observe(time.Since(start).Seconds())
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

// statusRecorder remembers the status code a handler wrote.
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
