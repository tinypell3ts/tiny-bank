package kit

import (
	"context"
	"regexp"
)

// RequestIDHeader is read from callers and echoed back.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// RequestIDFrom returns the request's ID, which kit.Handler stores in the
// request context, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// requestIDPattern is 1 to 64 ASCII letters, digits, '-' or '_'. Go's $ matches
// only the very end of the string, so a trailing newline is rejected too.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validRequestID accepts only short IDs of letters, digits, '-' and '_', so a caller
// can't inject newlines or huge values into our logs.
func validRequestID(id string) bool {
	return requestIDPattern.MatchString(id)
}
