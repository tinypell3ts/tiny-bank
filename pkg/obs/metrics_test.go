package obs

import (
	"fmt"
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

func TestMiddlewareBoundsUnknownMethods(t *testing.T) {
	_, m, h := newTestServer(t)
	for i := range 50 {
		do(t, h, fmt.Sprintf("CUSTOM%d", i), "/accounts/1")
	}

	if got := sampleCount(t, m.duration.WithLabelValues("_OTHER", "unmatched", "405")); got != 50 {
		t.Errorf("_OTHER requests = %v, want 50", got)
	}
	if n := testutil.CollectAndCount(m.duration); n != 1 {
		t.Errorf("duration has %d series, want 1: unknown methods must share one label", n)
	}
}

func TestStatusRecorder(t *testing.T) {
	tests := []struct {
		name    string
		handler func(w http.ResponseWriter)
		want    int
	}{
		{"nothing written is an implicit 200", func(http.ResponseWriter) {}, 200},
		{"body without WriteHeader is an implicit 200", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, "hi")
		}, 200},
		{"explicit status", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTeapot) }, 418},
		{"1xx is skipped for the final status", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			_, _ = io.WriteString(w, "body") // client gets 200
		}, 200},
		{"1xx then explicit final status", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusCreated)
		}, 201},
		{"a second WriteHeader is ignored, as net/http does", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			w.WriteHeader(http.StatusOK)
		}, 500},
		{"WriteHeader after the body is ignored", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, "body")
			w.WriteHeader(http.StatusInternalServerError)
		}, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A real server, so we compare with what the client actually receives.
			// The handler runs on the server's goroutine, so the recorded status
			// comes back over a channel: receiving the HTTP response alone doesn't
			// order the two goroutines' memory accesses.
			recorded := make(chan int, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rec := NewStatusRecorder(w)
				tt.handler(rec)
				recorded <- rec.Status()
			}))
			defer srv.Close()

			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != tt.want {
				t.Fatalf("client got %d, test expects %d", resp.StatusCode, tt.want)
			}
			if got := <-recorded; got != resp.StatusCode {
				t.Errorf("recorded %d, client got %d", got, resp.StatusCode)
			}
		})
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
