package kit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
)

// MaxBodyBytes caps a JSON request body. Requests are small, so this is
// generous, and stops a caller tying up memory with a huge body.
const MaxBodyBytes = 16 << 10

// DecodeJSON reads req's body into v, strictly. Any failure is a terrors
// bad_request with a message that's safe to show the caller:
//
//   - the Content-Type must be application/json (bad_request.content_type),
//     so a browser can't send it cross-site as a "simple" text/plain request
//   - the body must be at most MaxBodyBytes (bad_request.too_large)
//   - it must be exactly one JSON object, with no fields v doesn't have and
//     nothing after it (bad_request.invalid_json), so a typo like "currancy"
//     is an error instead of being silently ignored, and the message names it
//   - a body that can't be read, such as an upload cut off part way, is the
//     caller's fault too (bad_request.unreadable_body), not a server error
//
// Typhon's own req.Decode is lenient on all of these, so services use this.
func DecodeJSON(req typhon.Request, v any) error {
	if mediaType, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type")); mediaType != "application/json" {
		return terrors.BadRequest("content_type", "Content-Type must be application/json", nil)
	}
	if req.Body == nil {
		return notAnObject()
	}
	// kit.Handler already caps the body with http.MaxBytesReader; the
	// LimitReader keeps DecodeJSON safe on its own too.
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxBodyBytes+1))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge) || len(body) > MaxBodyBytes:
		return terrors.BadRequest("too_large", "request body is too large", nil)
	case err != nil:
		// If the caller has gone, kit.Handler answers 499 instead.
		return terrors.BadRequest("unreadable_body", "request body could not be read", nil)
	}

	// json.Decode accepts null for a struct and leaves it unchanged, so check
	// for an object first.
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return notAnObject()
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return terrors.BadRequest("invalid_json", decodeMessage(err), nil)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return terrors.BadRequest("invalid_json", "request body must be a single JSON object", nil)
	}
	return nil
}

func notAnObject() error {
	return terrors.BadRequest("invalid_json", "request body must be a JSON object", nil)
}

// fieldName is what a JSON field name must look like to be repeated back in
// an error. The name comes from the caller, so anything else (huge, or with
// odd characters) is left out of the message.
var fieldName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// decodeMessage says what was wrong with the body in words safe to show the
// caller. It never repeats a value they sent, only a field name.
func decodeMessage(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fmt.Sprintf("field %q has the wrong type", typeErr.Field) // Field is v's own path, not caller text
	}
	// encoding/json has no typed error for an unknown field, only this text.
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		if name, err := strconv.Unquote(name); err == nil && fieldName.MatchString(name) {
			return fmt.Sprintf("request body has an unknown field %q", name)
		}
		return "request body has an unknown field"
	}
	return "request body must be valid JSON"
}
