package kit

import "log/slog"

// Secret holds a value that must never be printed or logged, such as
// DATABASE_URL, which contains the database password.
//
// Every way Go turns a value into text shows "xxxxx" instead: fmt (%v, %s,
// %q, %+v, %#v, including inside structs and errors), log/slog, and JSON.
// So logging a whole config struct, or wrapping it in an error, can't leak
// the secret. The only way to get the value is Reveal, which makes using a
// secret a deliberate act that's easy to find in review.
type Secret string

const hidden = "xxxxx"

// Reveal returns the secret's value. Call it only where the value is used
// (opening a database connection, say), never to log or return it.
func (s Secret) Reveal() string { return string(s) }

// String hides the value from fmt's %v, %s and %q.
func (Secret) String() string { return hidden }

// GoString hides the value from fmt's %#v.
func (Secret) GoString() string { return hidden }

// LogValue hides the value from log/slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(hidden) }

// MarshalText hides the value from encoding/json and other text encoders.
func (Secret) MarshalText() ([]byte, error) { return []byte(hidden), nil }
