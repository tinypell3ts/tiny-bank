// Package obs holds the observability building blocks shared by every service.
// For now that is Prometheus metrics for HTTP servers. Logging and tracing join
// it in later phases, when a second service needs them.
package obs

import (
	"net/http"
	"strconv"
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
// ends, with its method, route pattern and status. kit.Handler calls it for
// every request, so services never record HTTP metrics themselves.
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

// routeOf is the route label: the pattern that matched, or "unmatched" when
// nothing did (a 404), so unknown paths never become labels.
func routeOf(pattern string) string {
	if pattern == "" {
		return "unmatched"
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
