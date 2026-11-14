package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// testServer builds a Handler with routes that exercise each filter, and
// returns it with its captured logs.
func testServer(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	router := typhon.Router{}
	router.GET("/healthz", Health)
	router.GET("/things/:id", func(req typhon.Request) typhon.Response {
		return req.Response(map[string]string{"id": typhon.RouterForRequest(req).Params(req)["id"]})
	})
	router.GET("/missing", func(req typhon.Request) typhon.Response {
		return typhon.Response{Error: terrors.NotFound("thing", "thing not found", nil)}
	})
	router.GET("/wrapped", func(req typhon.Request) typhon.Response {
		return typhon.Response{Error: fmt.Errorf("get thing: %w", terrors.BadRequest("bad_id", "id must be a number", nil))}
	})
	router.GET("/raw", func(req typhon.Request) typhon.Response {
		return typhon.Response{Error: errors.New("pq: connection to 10.0.0.7 failed: relation accounts_tmp does not exist")}
	})
	router.GET("/internal", func(req typhon.Request) typhon.Response {
		return typhon.Response{Error: terrors.InternalService("db", "database unavailable", nil)}
	})
	router.GET("/panic", func(req typhon.Request) typhon.Response { panic("boom: secret detail") })
	router.GET("/wait", func(req typhon.Request) typhon.Response {
		<-req.Done() // like a database query cut short when the caller leaves
		return typhon.Response{Error: fmt.Errorf("get thing: %w", req.Err())}
	})
	router.POST("/things", func(req typhon.Request) typhon.Response {
		var body struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(req, &body); err != nil {
			return typhon.Response{Error: err}
		}
		return req.Response(body)
	})
	router.GET("/cancelled-inside", func(req typhon.Request) typhon.Response {
		return typhon.Response{Error: fmt.Errorf("get thing: %w", context.Canceled)}
	})
	h := Handler(slog.New(slog.NewJSONHandler(&logs, nil)), obs.NewRegistry(), &router)
	return h, &logs
}

func do(t *testing.T, h http.Handler, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerRoutesAndHealth(t *testing.T) {
	h, _ := testServer(t)
	rec := do(t, h, "/healthz", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("/healthz = %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "/things/42", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"42"`) {
		t.Errorf("/things/42 = %d %s", rec.Code, rec.Body)
	}
}

func TestErrorsReachTheCallerSafely(t *testing.T) {
	tests := []struct {
		path        string
		wantStatus  int
		wantCode    string // terror code in the body
		wantMessage string
	}{
		{"/missing", http.StatusNotFound, "not_found.thing", "thing not found"},
		// A terror wrapped with fmt.Errorf keeps its code and message.
		{"/wrapped", http.StatusBadRequest, "bad_request.bad_id", "id must be a number"},
		// A raw error becomes a generic internal error.
		{"/raw", http.StatusInternalServerError, "internal_service", "internal error"},
		{"/internal", http.StatusInternalServerError, "internal_service.db", "database unavailable"},
		{"/panic", http.StatusInternalServerError, "internal_service.panic", "internal error"},
		{"/no-such-route", http.StatusNotFound, "not_found.no_handler", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			h, _ := testServer(t)
			rec := do(t, h, tt.path, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body isn't JSON: %v: %s", err, rec.Body)
			}
			if body["code"] != tt.wantCode {
				t.Errorf("code = %v, want %q", body["code"], tt.wantCode)
			}
			if tt.wantMessage != "" && body["message"] != tt.wantMessage {
				t.Errorf("message = %v, want %q", body["message"], tt.wantMessage)
			}
			// Nothing internal ever reaches the caller.
			for _, leak := range []string{"stack", "filename", "accounts_tmp", "10.0.0.7", "boom", "get thing:", "/Users/"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("response leaks %q: %s", leak, rec.Body)
				}
			}
		})
	}
}

func TestInternalErrorsAreLogged(t *testing.T) {
	for path, want := range map[string]string{
		"/raw":      "relation accounts_tmp does not exist", // the full raw error goes to the logs
		"/internal": "database unavailable",
		"/panic":    "boom: secret detail",
	} {
		h, logs := testServer(t)
		do(t, h, path, nil)
		if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), want) {
			t.Errorf("%s: want an ERROR log containing %q, got:\n%s", path, want, logs)
		}
	}
	// A deliberate 4xx is the caller's mistake: no error log.
	h, logs := testServer(t)
	do(t, h, "/missing", nil)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a 404 shouldn't log an error:\n%s", logs)
	}
}

func TestRequestIDFilter(t *testing.T) {
	h, logs := testServer(t)

	rec := do(t, h, "/healthz", http.Header{RequestIDHeader: {"abc-123"}})
	if got := rec.Header().Get(RequestIDHeader); got != "abc-123" {
		t.Errorf("valid caller ID not reused: got %q", got)
	}
	if !strings.Contains(logs.String(), `"request_id":"abc-123"`) {
		t.Errorf("access log missing the request ID:\n%s", logs)
	}

	rec = do(t, h, "/healthz", nil)
	if got := rec.Header().Get(RequestIDHeader); !validRequestID(got) {
		t.Errorf("a request without an ID should get a new valid one, got %q", got)
	}
	rec = do(t, h, "/healthz", http.Header{RequestIDHeader: {"bad\nid"}})
	if got := rec.Header().Get(RequestIDHeader); !validRequestID(got) || got == "bad\nid" {
		t.Errorf("invalid caller ID should be replaced, got %q", got)
	}
	// Errors carry the ID too, so a failure can be found from the caller's ID.
	rec = do(t, h, "/missing", http.Header{RequestIDHeader: {"err-1"}})
	if got := rec.Header().Get(RequestIDHeader); got != "err-1" {
		t.Errorf("error response request ID = %q, want err-1", got)
	}
}

func TestAccessLogLine(t *testing.T) {
	h, logs := testServer(t)
	do(t, h, "/things/7", nil)
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log isn't one JSON line: %v\n%s", err, logs)
	}
	if entry["msg"] != "http request" || entry["method"] != "GET" || entry["path"] != "/things/7" || entry["status"] != float64(200) {
		t.Errorf("access log = %v", entry)
	}
}

func TestMetricsUseRoutePatterns(t *testing.T) {
	h, _ := testServer(t)
	do(t, h, "/things/1", nil)
	do(t, h, "/things/2", nil)
	do(t, h, "/missing", nil)
	do(t, h, "/no-such-route", nil)
	do(t, h, "/panic", nil)
	wrongMethod := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/things/1", nil)
	h.ServeHTTP(httptest.NewRecorder(), wrongMethod)

	body := do(t, h, "/metrics", nil).Body.String()
	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/things/:id"} 2`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="/missing"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="unmatched"} 1`,
		// Typhon's router has no 405: a route with the wrong method matches nothing.
		`http_server_request_duration_seconds_count{http_request_method="POST",http_response_status_code="404",http_route="unmatched"} 1`,
		// A panic still ends its request in the metrics, and the gauge comes back down.
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="500",http_route="/panic"} 1`,
		"http_server_active_requests 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %s", want)
		}
	}
	if strings.Contains(body, "/things/1") || strings.Contains(body, "no-such-route") {
		t.Error("/metrics must not contain raw paths")
	}
	if strings.Contains(body, `http_route="/metrics"`) {
		t.Error("Prometheus scrapes shouldn't be counted as traffic")
	}
}

func TestCallerGoneIs499NotAnError(t *testing.T) {
	h, logs := testServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the caller hung up
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/wait", nil))

	if rec.Code != statusClientClosedRequest {
		t.Errorf("status = %d, want 499", rec.Code)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a caller leaving isn't a server error, but it was logged as one:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "request cancelled before it finished") {
		t.Errorf("the cancellation should be logged at info:\n%s", logs)
	}
}

func TestCancelledInsideIsAServerError(t *testing.T) {
	// The caller is still waiting, but something inside returned
	// context.Canceled. Only the request's own context decides 499.
	h, logs := testServer(t)
	if rec := do(t, h, "/cancelled-inside", nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("want an error log:\n%s", logs)
	}
}

func TestHandlerCapsRequestBodies(t *testing.T) {
	h, logs := testServer(t)
	for size, want := range map[int]int{
		MaxBodyBytes:     http.StatusOK,
		MaxBodyBytes + 1: http.StatusBadRequest,
		4 * MaxBodyBytes: http.StatusBadRequest,
	} {
		body := `{"name":"` + strings.Repeat("a", size-len(`{"name":""}`)) + `"}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/things", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%d-byte body: status %d, want %d: %s", size, rec.Code, want, rec.Body)
		}
		if want == http.StatusBadRequest && !strings.Contains(rec.Body.String(), "bad_request.too_large") {
			t.Errorf("%d-byte body: want bad_request.too_large, got %s", size, rec.Body)
		}
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a too-large body is the caller's fault, not a server error:\n%s", logs)
	}
}

func TestHandlerClosesTheConnectionAfterAHugeBody(t *testing.T) {
	// http.MaxBytesReader tells net/http to close the connection, rather than
	// read and discard the rest of a huge body to reuse it.
	h, _ := testServer(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	body := `{"name":"` + strings.Repeat("a", 4*MaxBodyBytes) + `"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/things", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	rsp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rsp.Body.Close() }()
	if rsp.StatusCode != http.StatusBadRequest || !rsp.Close {
		t.Errorf("status %d, Connection: close %v; want 400 and the connection closed", rsp.StatusCode, rsp.Close)
	}
}
