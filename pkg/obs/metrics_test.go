package obs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// record tracks one finished request, as kit.Handler does.
func record(m *HTTPMetrics, method, route string, status int) {
	done := m.Track()
	done(method, route, status)
}

func TestTrackCountsByRouteAndCode(t *testing.T) {
	m := NewHTTPMetrics(NewRegistry())

	record(m, http.MethodGet, "/accounts/:id", 200)
	record(m, http.MethodGet, "/accounts/:id", 200)
	record(m, http.MethodGet, "/boom", 500)
	record(m, http.MethodGet, "", 404) // matched no route

	tests := []struct {
		name                string
		method, route, code string
		want                float64
	}{
		{"two IDs share one route series", "GET", "/accounts/:id", "200", 2},
		{"server errors keep their code", "GET", "/boom", "500", 1},
		{"unknown paths are not used as labels", "GET", "unmatched", "404", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sampleCount(t, m.duration.WithLabelValues(tt.method, tt.route, tt.code))
			if got != tt.want {
				t.Errorf("requests{%s %s %s} = %v, want %v", tt.method, tt.route, tt.code, got, tt.want)
			}
		})
	}
	if n := testutil.CollectAndCount(m.duration); n != 3 {
		t.Errorf("duration has %d series, want 3 (one per method/route/code)", n)
	}
}

func TestTrackCountsActiveRequests(t *testing.T) {
	m := NewHTTPMetrics(NewRegistry())
	done := m.Track()
	if got := testutil.ToFloat64(m.active); got != 1 {
		t.Errorf("active = %v while handling, want 1", got)
	}
	done(http.MethodGet, "/", 200)
	if got := testutil.ToFloat64(m.active); got != 0 {
		t.Errorf("active = %v after the request finished, want 0", got)
	}
}

func TestTrackBoundsUnknownMethods(t *testing.T) {
	m := NewHTTPMetrics(NewRegistry())
	for i := range 50 {
		record(m, fmt.Sprintf("CUSTOM%d", i), "", 404)
	}
	if got := sampleCount(t, m.duration.WithLabelValues("_OTHER", "unmatched", "404")); got != 50 {
		t.Errorf("_OTHER requests = %v, want 50", got)
	}
	if n := testutil.CollectAndCount(m.duration); n != 1 {
		t.Errorf("duration has %d series, want 1: unknown methods must share one label", n)
	}
}

func TestHandlerExposesMetrics(t *testing.T) {
	reg := NewRegistry()
	record(NewHTTPMetrics(reg), http.MethodGet, "/accounts/:id", 200)

	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/accounts/:id"} 1`,
		`http_server_request_duration_seconds_bucket{http_request_method="GET",http_response_status_code="200",http_route="/accounts/:id",le="0.001"}`,
		"http_server_active_requests 0",
		"go_goroutines",             // Go runtime metrics from NewRegistry
		"process_cpu_seconds_total", // process metrics from NewRegistry
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func sampleCount(t *testing.T, o prometheus.Observer) float64 {
	t.Helper()
	var pb dto.Metric
	if err := o.(prometheus.Metric).Write(&pb); err != nil {
		t.Fatal(err)
	}
	return float64(pb.GetHistogram().GetSampleCount())
}
