package obs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// newTestServer wires a small mux behind the middleware, like a real service.
func newTestServer(t *testing.T) (*prometheus.Registry, *HTTPMetrics, http.Handler) {
	t.Helper()
	reg := NewRegistry()
	m := NewHTTPMetrics(reg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /implicit-ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "no WriteHeader call") // status defaults to 200
	})
	return reg, m, m.Middleware(mux)
}

func do(t *testing.T, h http.Handler, method, path string) {
	t.Helper()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), method, path, nil))
}

func TestMiddlewareCountsByRouteAndCode(t *testing.T) {
	_, m, h := newTestServer(t)

	do(t, h, http.MethodGet, "/accounts/1")
	do(t, h, http.MethodGet, "/accounts/2")
	do(t, h, http.MethodGet, "/boom")
	do(t, h, http.MethodGet, "/implicit-ok")
	do(t, h, http.MethodGet, "/no-such-route")
	do(t, h, http.MethodPost, "/accounts/1") // route exists, method doesn't

	tests := []struct {
		name                string
		method, route, code string
		want                float64
	}{
		{"two IDs share one route series", "GET", "/accounts/{id}", "200", 2},
		{"server errors keep their code", "GET", "/boom", "500", 1},
		{"implicit 200 is recorded", "GET", "/implicit-ok", "200", 1},
		{"unknown paths are not used as labels", "GET", "unmatched", "404", 1},
		{"wrong method is recorded", "POST", "unmatched", "405", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sampleCount(t, m.duration.WithLabelValues(tt.method, tt.route, tt.code))
			if got != tt.want {
				t.Errorf("requests{%s %s %s} = %v, want %v", tt.method, tt.route, tt.code, got, tt.want)
			}
		})
	}

	// The raw paths must never appear as label values.
	if n := testutil.CollectAndCount(m.duration); n != 5 {
		t.Errorf("duration has %d series, want 5 (one per method/route/code)", n)
	}
}

func TestMiddlewareTracksActiveRequests(t *testing.T) {
	reg := NewRegistry()
	m := NewHTTPMetrics(reg)
	var during float64
	h := m.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		during = testutil.ToFloat64(m.active)
	}))
	do(t, h, http.MethodGet, "/")

	if during != 1 {
		t.Errorf("active = %v while handling, want 1", during)
	}
	if got := testutil.ToFloat64(m.active); got != 0 {
		t.Errorf("active = %v after the request finished, want 0", got)
	}
}

func TestHandlerExposesMetrics(t *testing.T) {
	reg, _, h := newTestServer(t)
	do(t, h, http.MethodGet, "/accounts/1")

	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/accounts/{id}"} 1`,
		`http_server_request_duration_seconds_bucket{http_request_method="GET",http_response_status_code="200",http_route="/accounts/{id}",le="0.001"}`,
		"http_server_active_requests 0",
		"go_goroutines",             // Go runtime metrics from NewRegistry
		"process_cpu_seconds_total", // process metrics from NewRegistry
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

// sampleCount returns how many observations a histogram series has, i.e. its
// _count: the number of requests it saw.
func sampleCount(t *testing.T, o prometheus.Observer) float64 {
	t.Helper()
	var pb dto.Metric
	if err := o.(prometheus.Metric).Write(&pb); err != nil {
		t.Fatal(err)
	}
	return float64(pb.GetHistogram().GetSampleCount())
}

func TestRouteOf(t *testing.T) {
	tests := map[string]string{
		"GET /accounts/{id}": "/accounts/{id}",
		"/healthz":           "/healthz",
		"GET /{$}":           "/{$}",
		"":                   "unmatched",
	}
	for pattern, want := range tests {
		if got := routeOf(pattern); got != want {
			t.Errorf("routeOf(%q) = %q, want %q", pattern, got, want)
		}
	}
}
