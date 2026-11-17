package kit

import (
	"errors"
	"strings"

	"github.com/monzo/terrors"
)

// refusalClasses are the terror classes that mean a request was refused for
// what it asked (a 4xx), so sending it again unchanged gets the same answer.
var refusalClasses = map[string]bool{
	terrors.ErrBadRequest:         true,
	terrors.ErrNotFound:           true,
	terrors.ErrPreconditionFailed: true,
}

// Refusal reports whether err is, or wraps, a terror refusing the request
// for good: bad_request, not_found or precondition_failed, such as
// precondition_failed.insufficient_funds. code is the terror's code.
//
// Anything else (a timeout, a 5xx, no answer at all) isn't a refusal: the
// request might have worked, or might work if tried again. The ledger counts
// refusals in its metrics with this, and the transaction service decides
// with it whether a transfer has failed for good or is worth retrying, so
// the two always agree.
func Refusal(err error) (code string, ok bool) {
	var terr *terrors.Error
	if !errors.As(err, &terr) || !refusalClasses[strings.SplitN(terr.Code, ".", 2)[0]] {
		return "", false
	}
	return terr.Code, true
}
