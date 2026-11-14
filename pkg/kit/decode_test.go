package kit

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
)

type thing struct {
	Name string `json:"name"`
}

func jsonRequest(t *testing.T, contentType string, body io.Reader) typhon.Request {
	t.Helper()
	req := typhon.NewRequest(t.Context(), http.MethodPost, "/things", nil)
	req.Body = nil
	if body != nil {
		req.Body = io.NopCloser(body)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantCode    string // terror code; empty means success
	}{
		{"one object", "application/json", `{"name":"Ada"}`, ""},
		{"media type parameters are fine", "application/json; charset=utf-8", `{"name":"Ada"}`, ""},
		{"exactly the limit", "application/json", `{"name":"` + strings.Repeat("a", MaxBodyBytes-len(`{"name":""}`)) + `"}`, ""},
		{"no Content-Type", "", `{"name":"Ada"}`, "bad_request.content_type"},
		{"text/plain (a cross-site simple request)", "text/plain", `{"name":"Ada"}`, "bad_request.content_type"},
		{"empty body", "application/json", ``, "bad_request.invalid_json"},
		{"not JSON", "application/json", `name=Ada`, "bad_request.invalid_json"},
		{"unknown field", "application/json", `{"name":"Ada","title":"Dr"}`, "bad_request.invalid_json"},
		{"wrong type", "application/json", `{"name":7}`, "bad_request.invalid_json"},
		{"two objects", "application/json", `{"name":"Ada"}{"name":"Bob"}`, "bad_request.invalid_json"},
		{"trailing junk", "application/json", `{"name":"Ada"} x`, "bad_request.invalid_json"},
		{"null is not an object", "application/json", `null`, "bad_request.invalid_json"},
		{"an array is not an object", "application/json", ` [{"name":"Ada"}]`, "bad_request.invalid_json"},
		{"whitespace only", "application/json", " \n ", "bad_request.invalid_json"},
		{"one byte over the limit", "application/json", `{"name":"` + strings.Repeat("a", MaxBodyBytes+1-len(`{"name":""}`)) + `"}`, "bad_request.too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got thing
			err := DecodeJSON(jsonRequest(t, tt.contentType, strings.NewReader(tt.body)), &got)
			if tt.wantCode == "" {
				if err != nil || got.Name == "" {
					t.Fatalf("err = %v, thing = %+v; want it decoded", err, got)
				}
				return
			}
			var terr *terrors.Error
			if !errors.As(err, &terr) || terr.Code != tt.wantCode {
				t.Fatalf("err = %v, want terror %s", err, tt.wantCode)
			}
			if strings.Contains(terr.Message, "json:") {
				t.Errorf("message leaks Go's decoder wording: %q", terr.Message)
			}
		})
	}
}

func TestDecodeJSONNilBody(t *testing.T) {
	var got thing
	err := DecodeJSON(jsonRequest(t, "application/json", nil), &got)
	if !terrors.PrefixMatches(err, "bad_request.invalid_json") {
		t.Errorf("err = %v, want bad_request.invalid_json", err)
	}
}

func TestDecodeJSONMessagesSayWhatIsWrong(t *testing.T) {
	tests := map[string]string{
		`{"title":"Dr"}`:                        `request body has an unknown field "title"`,
		`{"` + strings.Repeat("x", 65) + `":1}`: "request body has an unknown field", // too long to repeat back
		`{"na\nme":1}`:                          "request body has an unknown field", // odd characters aren't repeated back
		`{"name":7}`:                            `field "name" has the wrong type`,
		`{"name":`:                              "request body must be valid JSON",
	}
	for body, want := range tests {
		var got thing
		err := DecodeJSON(jsonRequest(t, "application/json", strings.NewReader(body)), &got)
		var terr *terrors.Error
		if !errors.As(err, &terr) || terr.Message != want {
			t.Errorf("DecodeJSON(%s) = %v, want message %q", body, err, want)
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestDecodeJSONReadErrors(t *testing.T) {
	tests := map[string]struct {
		err      error
		wantCode string
	}{
		// An upload cut off part way is the caller's fault: a 400, not a 500.
		"cut off":                     {io.ErrUnexpectedEOF, "bad_request.unreadable_body"},
		"over kit.Handler's body cap": {&http.MaxBytesError{Limit: MaxBodyBytes}, "bad_request.too_large"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var got thing
			err := DecodeJSON(jsonRequest(t, "application/json", failingReader{tt.err}), &got)
			var terr *terrors.Error
			if !errors.As(err, &terr) || terr.Code != tt.wantCode {
				t.Errorf("err = %v, want %s", err, tt.wantCode)
			}
		})
	}
}
