package kit

import (
	"errors"
	"strings"
	"testing"

	"github.com/monzo/terrors"
)

func TestCheckIdempotencyKey(t *testing.T) {
	for _, good := range []string{"t1", "c0ffee00-0000-4000-8000-000000000001", "Aa0._:-", strings.Repeat("k", MaxIdempotencyKeyLen)} {
		if err := CheckIdempotencyKey(good); err != nil {
			t.Errorf("CheckIdempotencyKey(%q) = %v, want ok", good, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("k", MaxIdempotencyKeyLen+1), "k 1", "k\n1", "k/1"} {
		var terr *terrors.Error
		if err := CheckIdempotencyKey(bad); !errors.As(err, &terr) || terr.Code != "bad_request.idempotency_key" {
			t.Errorf("CheckIdempotencyKey(%q) = %v, want bad_request.idempotency_key", bad, err)
		}
	}
}
