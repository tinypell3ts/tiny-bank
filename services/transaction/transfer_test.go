package transaction

import (
	"errors"
	"testing"
	"uuid"

	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

var (
	alice = uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("b0b00000-0000-4000-8000-000000000002")
)

func gbp(minor int64) money.Money { return money.New(minor, money.GBP()) }

func transferReq(key string, from, to uuid.UUID, minor int64) NewTransfer {
	return NewTransfer{IdempotencyKey: key, Kind: KindTransfer, From: from, To: to, Amount: gbp(minor)}
}

func depositReq(key string, to uuid.UUID, minor int64) NewTransfer {
	return NewTransfer{IdempotencyKey: key, Kind: KindDeposit, To: to, Amount: gbp(minor)}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		req      NewTransfer
		wantCode string // empty means valid
	}{
		{"a transfer", transferReq("t1", alice, bob, 300), ""},
		{"a deposit", depositReq("d1", alice, 1000), ""},
		{"the largest amount", transferReq("t1", alice, bob, maxAmount), ""},

		{"no key", transferReq("", alice, bob, 300), "bad_request.idempotency_key"},
		{"an unknown kind", NewTransfer{IdempotencyKey: "t1", Kind: "refund", From: alice, To: bob, Amount: gbp(300)}, "bad_request.kind"},
		{"a transfer with no from", transferReq("t1", uuid.Nil(), bob, 300), "bad_request.from_account_id"},
		{"a deposit with a from", NewTransfer{IdempotencyKey: "d1", Kind: KindDeposit, From: bob, To: alice, Amount: gbp(300)}, "bad_request.from_account_id"},
		{"no to", transferReq("t1", alice, uuid.Nil(), 300), "bad_request.to_account_id"},
		{"a deposit with no account", depositReq("d1", uuid.Nil(), 300), "bad_request.account_id"},
		{"to itself", transferReq("t1", alice, alice, 300), "bad_request.to_account_id"},
		{"no currency", NewTransfer{IdempotencyKey: "t1", Kind: KindTransfer, From: alice, To: bob}, "bad_request.currency"},
		{"zero", transferReq("t1", alice, bob, 0), "bad_request.amount"},
		{"negative", transferReq("t1", alice, bob, -300), "bad_request.amount"},
		{"over the limit", transferReq("t1", alice, bob, maxAmount+1), "bad_request.amount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("err = %v, want valid", err)
				}
				return
			}
			var terr *terrors.Error
			if !errors.As(err, &terr) || terr.Code != tt.wantCode {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
		})
	}
}
