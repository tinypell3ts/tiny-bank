package kit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzAndWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	Healthz(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %s", got)
	}
}

func TestAccessLogSeesTheFinalStatus(t *testing.T) {
	var logs bytes.Buffer
	h := RequestID(AccessLog(slog.New(slog.NewJSONHandler(&logs, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusTeapot, map[string]string{"status": "short and stout"})
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/brew", nil))

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log is not one JSON line: %v\n%s", err, logs.String())
	}
	if entry["status"] != float64(http.StatusTeapot) || entry["method"] != "POST" || entry["path"] != "/brew" {
		t.Errorf("log entry = %v", entry)
	}
}

func TestRequestIDAndAccessLog(t *testing.T) {
	tests := []struct {
		name     string
		incoming string // X-Request-Id sent by the caller
		wantSame bool   // expect the incoming ID to be reused
	}{
		{name: "generated when missing", incoming: ""},
		{name: "reused when valid", incoming: "abc-123_XYZ", wantSame: true},
		{name: "replaced when it contains a newline", incoming: "bad\nid"},
		{name: "replaced when too long", incoming: strings.Repeat("a", 65)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := RequestID(AccessLog(slog.New(slog.NewJSONHandler(&logs, nil)), http.HandlerFunc(Healthz)))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(RequestIDHeader)
			if !validRequestID(got) {
				t.Fatalf("response request ID %q is not valid", got)
			}
			if tt.wantSame && got != tt.incoming {
				t.Errorf("request ID = %q, want caller's %q", got, tt.incoming)
			}
			if !tt.wantSame && got == tt.incoming {
				t.Errorf("request ID %q should have been replaced", got)
			}

			// The access log line must carry the same ID, so logs can be joined to a request.
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("log is not one JSON line: %v\n%s", err, logs.String())
			}
			if entry["request_id"] != got {
				t.Errorf("logged request_id = %v, want %q", entry["request_id"], got)
			}
			if entry["status"] != float64(http.StatusOK) || entry["path"] != "/healthz" || entry["method"] != "GET" {
				t.Errorf("log entry missing request details: %v", entry)
			}
		})
	}
}

func TestValidRequestID(t *testing.T) {
	tests := map[string]bool{
		"abc-123_XYZ":           true,
		strings.Repeat("a", 64): true, // exactly the limit
		"":                      false,
		strings.Repeat("a", 65): false, // one over
		"abc\n":                 false, // a trailing newline must not slip through $
		"a b":                   false,
		"café":                  false, // ASCII only
		"id;drop":               false,
	}
	for id, want := range tests {
		if got := validRequestID(id); got != want {
			t.Errorf("validRequestID(%q) = %v, want %v", id, got, want)
		}
	}
}
