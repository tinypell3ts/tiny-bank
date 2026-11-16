package kit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"regexp"
	"slices"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// MaxBodyBytes caps a request body. Requests are small, so this is
// generous, and stops a caller tying up memory with a huge body.
const MaxBodyBytes = 16 << 10

// Decode reads req's JSON body into m, the service's protobuf request
// message, strictly. Fields use the proto names (customer_id); the JSON
// names (customerId) are accepted too. Any failure is a terrors bad_request
// with a message that's safe to show the caller:
//
//   - the Content-Type must be application/json (bad_request.content_type),
//     so a browser can't send it cross-site as a "simple" text/plain request
//   - the body must be at most MaxBodyBytes (bad_request.too_large)
//   - a body that can't be read, such as an upload cut off part way, is the
//     caller's fault too (bad_request.unreadable_body), not a server error
//   - it must be one JSON object, with only fields m has, each with a valid
//     value (bad_request.invalid_json), so a typo like "currancy" is an error
//     instead of being silently ignored, and the message names it
//
// Being strict means a new request field must reach the service before
// callers send it (proto/AGENTS.md). Typhon's own req.Decode is lenient on
// all of these, so services use this.
func Decode(req typhon.Request, m proto.Message) error {
	if mediaType, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type")); mediaType != "application/json" {
		return terrors.BadRequest("content_type", "Content-Type must be application/json", nil)
	}
	body, err := readBody(req)
	if err != nil {
		return err
	}
	// protojson rejects null and empty bodies too, but this says so plainly.
	if !isJSONObject(body) {
		return terrors.BadRequest("invalid_json", "request body must be a JSON object", nil)
	}
	if err := protojson.Unmarshal(body, m); err != nil { // strict: unknown fields are errors
		return terrors.BadRequest("invalid_json", invalidJSONMessage(body, m), nil)
	}
	return nil
}

// readBody reads req's whole body, at most MaxBodyBytes. A missing body is
// empty. Failures are bad_requests: too_large, or unreadable_body for a body
// cut off part way (if the caller has gone, kit.Handler answers 499 instead).
func readBody(req typhon.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	// kit.Handler already caps the body with http.MaxBytesReader; the
	// LimitReader keeps Decode safe on its own too.
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxBodyBytes+1))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge) || len(body) > MaxBodyBytes:
		return nil, terrors.BadRequest("too_large", "request body is too large", nil)
	case err != nil:
		return nil, terrors.BadRequest("unreadable_body", "request body could not be read", nil)
	}
	return body, nil
}

func isJSONObject(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// fieldName is what a JSON field name must look like to be repeated back in
// an error. The name comes from the caller, so anything else (huge, or with
// odd characters) is left out of the message.
var fieldName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// invalidJSONMessage says what was wrong with a body that failed to decode
// into m. An unknown field, the usual mistake (a typo), is named. protojson's
// own error text is deliberately unstable, so rather than parse it, this
// looks for a top-level field m doesn't have. It only runs after a failure.
func invalidJSONMessage(body []byte, m proto.Message) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "request body must be valid JSON, with one object"
	}
	known := m.ProtoReflect().Descriptor().Fields()
	for _, name := range slices.Sorted(maps.Keys(fields)) { // sorted, so a body always gets the same message
		if known.ByTextName(name) == nil && known.ByJSONName(name) == nil {
			if !fieldName.MatchString(name) {
				return "request body has an unknown field"
			}
			return fmt.Sprintf("request body has an unknown field %q", name)
		}
	}
	return "request body's fields must each appear once, with a value of the right type"
}
