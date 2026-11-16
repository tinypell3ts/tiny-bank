package kit

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"google.golang.org/protobuf/types/known/typepb"
)

// The tests decode into protobuf's own typepb.Type, so pkg/kit doesn't depend
// on any service's contract. It has text (name), enum (syntax), repeated
// message (fields) and message (source_context) fields.

func bodyRequest(t *testing.T, contentType string, body io.Reader) typhon.Request {
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

func TestDecode(t *testing.T) {
	limit := func(extra int) string { // a body of exactly MaxBodyBytes+extra bytes
		return `{"name":"` + strings.Repeat("a", MaxBodyBytes+extra-len(`{"name":""}`)) + `"}`
	}
	tests := []struct {
		name        string
		contentType string
		body        string
		wantCode    string // empty means it decodes
		wantMessage string // checked when set
	}{
		{"proto field names", "application/json", `{"name":"Ada","syntax":"SYNTAX_PROTO3","source_context":{"file_name":"a.proto"}}`, "", ""},
		{"JSON names are accepted too", "application/json", `{"name":"Ada","sourceContext":{"fileName":"a.proto"}}`, "", ""},
		{"media type parameters are fine", "application/json; charset=utf-8", `{"name":"Ada"}`, "", ""},
		{"exactly the limit", "application/json", limit(0), "", ""},

		{"no Content-Type", "", `{"name":"Ada"}`, "bad_request.content_type", ""},
		{"text/plain (a cross-site simple request)", "text/plain", `{"name":"Ada"}`, "bad_request.content_type", ""},
		{"binary protobuf isn't served yet", "application/protobuf", `{"name":"Ada"}`, "bad_request.content_type", ""},
		{"one byte over the limit", "application/json", limit(1), "bad_request.too_large", ""},

		{"empty body", "application/json", ``, "bad_request.invalid_json", "request body must be a JSON object"},
		{"whitespace only", "application/json", " \n ", "bad_request.invalid_json", "request body must be a JSON object"},
		{"null", "application/json", `null`, "bad_request.invalid_json", "request body must be a JSON object"},
		{"array", "application/json", ` [{"name":"Ada"}]`, "bad_request.invalid_json", "request body must be a JSON object"},
		{"not JSON", "application/json", `{name=Ada`, "bad_request.invalid_json", "request body must be valid JSON, with one object"},
		{"two objects", "application/json", `{"name":"a"}{"name":"b"}`, "bad_request.invalid_json", "request body must be valid JSON, with one object"},
		{"trailing junk", "application/json", `{"name":"Ada"} x`, "bad_request.invalid_json", "request body must be valid JSON, with one object"},
		{"unknown field is named", "application/json", `{"name":"Ada","title":"Dr"}`, "bad_request.invalid_json", `request body has an unknown field "title"`},
		{"long unknown name isn't repeated", "application/json", `{"` + strings.Repeat("x", 65) + `":1}`, "bad_request.invalid_json", "request body has an unknown field"},
		{"odd unknown name isn't repeated", "application/json", `{"ti\ntle":1}`, "bad_request.invalid_json", "request body has an unknown field"},
		{"wrong type", "application/json", `{"name":7}`, "bad_request.invalid_json", "request body's fields must each appear once, with a value of the right type"},
		{"unknown enum value", "application/json", `{"syntax":"SYNTAX_PROTO9"}`, "bad_request.invalid_json", "request body's fields must each appear once, with a value of the right type"},
		{"field twice", "application/json", `{"name":"a","name":"b"}`, "bad_request.invalid_json", "request body's fields must each appear once, with a value of the right type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got typepb.Type
			err := Decode(bodyRequest(t, tt.contentType, strings.NewReader(tt.body)), &got)
			if tt.wantCode == "" {
				if err != nil || got.GetName() == "" {
					t.Fatalf("err = %v, got %v; want it decoded", err, &got)
				}
				return
			}
			var terr *terrors.Error
			if !errors.As(err, &terr) || terr.Code != tt.wantCode || (tt.wantMessage != "" && terr.Message != tt.wantMessage) {
				t.Fatalf("err = %v, want %s %q", err, tt.wantCode, tt.wantMessage)
			}
			if strings.Contains(terr.Message, "proto:") || strings.Contains(terr.Message, "json:") {
				t.Errorf("message leaks a decoder's own wording: %q", terr.Message)
			}
		})
	}
}

func TestDecodeNilBody(t *testing.T) {
	var got typepb.Type
	if err := Decode(bodyRequest(t, "application/json", nil), &got); !terrors.PrefixMatches(err, "bad_request.invalid_json") {
		t.Errorf("err = %v, want bad_request.invalid_json", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestDecodeReadErrors(t *testing.T) {
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
			var got typepb.Type
			err := Decode(bodyRequest(t, "application/json", failingReader{tt.err}), &got)
			var terr *terrors.Error
			if !errors.As(err, &terr) || terr.Code != tt.wantCode {
				t.Errorf("err = %v, want %s", err, tt.wantCode)
			}
		})
	}
}
