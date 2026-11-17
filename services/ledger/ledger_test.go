package ledger

import (
	"errors"
	"math"
	"strings"
	"testing"
	"uuid"

	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/funding"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

var (
	alice = uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("b0b00000-0000-4000-8000-000000000002")
	carol = uuid.MustParse("ca401000-0000-4000-8000-000000000003")
)

func gbp(minor int64) money.Money { return money.New(minor, money.GBP()) }

func TestValidate(t *testing.T) {
	transfer := []Entry{{alice, gbp(-500)}, {bob, gbp(500)}}
	tests := []struct {
		name    string
		key     string
		entries []Entry
		wantErr string // a terrors code, or empty for valid
	}{
		{"a transfer", "k1", transfer, ""},
		{"a fee: three entries", "k1", []Entry{{alice, gbp(-550)}, {bob, gbp(500)}, {carol, gbp(50)}}, ""},
		{"key with every allowed character", "Aa0._:-", transfer, ""},
		{"key at the limit", strings.Repeat("k", kit.MaxIdempotencyKeyLen), transfer, ""},

		{"no key", "", transfer, "bad_request.idempotency_key"},
		{"key too long", strings.Repeat("k", kit.MaxIdempotencyKeyLen+1), transfer, "bad_request.idempotency_key"},
		{"key with a space", "k 1", transfer, "bad_request.idempotency_key"},
		{"key with a newline", "k\n1", transfer, "bad_request.idempotency_key"},

		{"no entries", "k1", nil, "bad_request.entries"},
		{"one entry", "k1", []Entry{{alice, gbp(0)}}, "bad_request.entries"},
		{"too many entries", "k1", manyEntries(maxEntries + 1), "bad_request.entries"},
		{"doesn't add up to zero", "k1", []Entry{{alice, gbp(-500)}, {bob, gbp(499)}}, "bad_request.entries"},
		{"mixed currencies", "k1", []Entry{{alice, gbp(-500)}, {bob, money.New(500, money.EUR())}}, "bad_request.entries"},
		{"no currency", "k1", []Entry{{alice, money.Money{}}, {bob, money.Money{}}}, "bad_request.entries"},
		{"zero amount", "k1", []Entry{{alice, gbp(0)}, {bob, gbp(0)}}, "bad_request.entries"},
		{"no account", "k1", []Entry{{uuid.Nil(), gbp(-500)}, {bob, gbp(500)}}, "bad_request.entries"},
		{"same account twice", "k1", []Entry{{alice, gbp(-500)}, {alice, gbp(500)}}, "bad_request.entries"},
		{"amount at the limit", "k1", []Entry{{alice, gbp(-money.MaxAmount)}, {bob, gbp(money.MaxAmount)}}, ""},
		{"amount over the limit", "k1", []Entry{{alice, gbp(-money.MaxAmount - 1)}, {bob, gbp(money.MaxAmount + 1)}}, "bad_request.entries"},
		{"would overflow", "k1", []Entry{{alice, gbp(math.MaxInt64)}, {bob, gbp(1)}, {carol, gbp(math.MinInt64)}}, "bad_request.entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewJournal{IdempotencyKey: tt.key, Entries: tt.entries}.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want valid", err)
				}
				return
			}
			var terr *terrors.Error
			if !errors.As(err, &terr) || terr.Code != tt.wantErr {
				t.Fatalf("err = %v, want %s", err, tt.wantErr)
			}
		})
	}
}

// manyEntries returns n entries that add up to zero, each on its own account.
func manyEntries(n int) []Entry {
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{uuid.New(), gbp(1)}
	}
	entries[0].Amount = gbp(int64(1 - n))
	return entries
}

func TestValidateSortsWithoutTouchingTheCallersSlice(t *testing.T) {
	in := []Entry{{carol, gbp(50)}, {alice, gbp(-550)}, {bob, gbp(500)}}
	got, err := NewJournal{IdempotencyKey: "k1", Entries: in}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries[0].AccountID != alice || got.Entries[1].AccountID != bob || got.Entries[2].AccountID != carol {
		t.Errorf("entries = %v, want sorted by account ID", got.Entries)
	}
	if in[0].AccountID != carol {
		t.Error("Validate reordered the caller's slice")
	}
}

func TestFundingAccount(t *testing.T) {
	for _, c := range []money.Currency{money.GBP(), money.EUR(), money.USD(), money.JPY()} {
		if _, ok := funding.Account(c); !ok {
			t.Errorf("no funding account for %s", c)
		}
	}
	if _, ok := funding.Account(money.Currency{}); ok {
		t.Error("the zero currency has a funding account")
	}
}
