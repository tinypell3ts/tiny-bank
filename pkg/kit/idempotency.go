package kit

import (
	"fmt"
	"regexp"

	"github.com/monzo/terrors"
)

// MaxIdempotencyKeyLen bounds an idempotency key. A UUID is 36 characters.
const MaxIdempotencyKeyLen = 100

// idempotencyKeyPattern is what a key may contain: letters, digits and a few
// separators, so a key is safe to log.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// CheckIdempotencyKey checks a caller's idempotency key: required, at most
// MaxIdempotencyKeyLen characters, and only letters, digits and . _ : -.
// Every service that takes one (the ledger, the transaction service) applies
// the same rules, so a key that one accepts, the next accepts too. A bad key
// is a terrors bad_request.idempotency_key.
func CheckIdempotencyKey(key string) error {
	switch n := len(key); {
	case n == 0:
		return terrors.BadRequest("idempotency_key", "idempotency key is required", nil)
	case n > MaxIdempotencyKeyLen:
		return terrors.BadRequest("idempotency_key", fmt.Sprintf("idempotency key must be at most %d characters", MaxIdempotencyKeyLen), nil)
	case !idempotencyKeyPattern.MatchString(key):
		return terrors.BadRequest("idempotency_key", "idempotency key may only contain letters, digits and . _ : -", nil)
	}
	return nil
}
