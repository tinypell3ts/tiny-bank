package kit

import (
	"bytes"
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

	body := do(t, h, "/metrics", nil).Body.String()
	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/things/:id"} 2`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="/missing"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="unmatched"} 1`,
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
