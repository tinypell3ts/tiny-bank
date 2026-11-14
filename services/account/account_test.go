package account

import (
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// customer is a made-up customer ID for tests.
var customer = uuid.MustParse("c0ffee00-0000-4000-8000-000000000001")

func TestNewRequestValidate(t *testing.T) {
	tests := []struct {
		name     string
		req      NewRequest
		wantName string // after tidying; checked only when there's no error
		wantErr  string // substring; empty means valid
	}{
		{name: "valid", req: NewRequest{CustomerID: customer, HolderName: "Ada Lovelace", Currency: money.GBP()}, wantName: "Ada Lovelace"},
		{name: "surrounding spaces trimmed", req: NewRequest{CustomerID: customer, HolderName: "  Ada  ", Currency: money.EUR()}, wantName: "Ada"},
		{name: "accents and other scripts are fine", req: NewRequest{CustomerID: customer, HolderName: "Zoë 山田", Currency: money.JPY()}, wantName: "Zoë 山田"},
		{name: "Persian name with a zero-width non-joiner", req: NewRequest{CustomerID: customer, HolderName: "مهر\u200cنوش", Currency: money.GBP()}, wantName: "مهر\u200cنوش"},
		{name: "exactly 200 characters", req: NewRequest{CustomerID: customer, HolderName: strings.Repeat("é", 200), Currency: money.GBP()}, wantName: strings.Repeat("é", 200)},
		{name: "empty name", req: NewRequest{CustomerID: customer, HolderName: "", Currency: money.GBP()}, wantErr: "holder name is required"},
		{name: "only spaces", req: NewRequest{CustomerID: customer, HolderName: "   ", Currency: money.GBP()}, wantErr: "holder name is required"},
		// 201 two-byte characters is 402 bytes: the limit must count characters.
		{name: "201 characters", req: NewRequest{CustomerID: customer, HolderName: strings.Repeat("é", 201), Currency: money.GBP()}, wantErr: "at most 200 characters"},
		{name: "newline in name", req: NewRequest{CustomerID: customer, HolderName: "Ada\nLovelace", Currency: money.GBP()}, wantErr: "control characters"},
		{name: "invalid UTF-8", req: NewRequest{CustomerID: customer, HolderName: "Ada\xff", Currency: money.GBP()}, wantErr: "valid UTF-8"},
		{name: "tab in name", req: NewRequest{CustomerID: customer, HolderName: "Ada\tLovelace", Currency: money.GBP()}, wantErr: "control characters"},
		{name: "no customer", req: NewRequest{HolderName: "Ada", Currency: money.GBP()}, wantErr: "customer ID is required"},
		{name: "no currency", req: NewRequest{CustomerID: customer, HolderName: "Ada", Currency: money.Currency{}}, wantErr: "currency is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.req.Validate()
			if tt.wantErr != "" {
				var terr *terrors.Error
				if !errors.As(err, &terr) || !terr.PrefixMatches(terrors.ErrBadRequest) || !strings.Contains(terr.Message, tt.wantErr) {
					t.Fatalf("err = %v, want a terrors bad_request whose message contains %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.CustomerID != customer || got.HolderName != tt.wantName || got.Currency != tt.req.Currency {
				t.Errorf("got %+v, want name %q and currency %v", got, tt.wantName, tt.req.Currency)
			}
		})
	}
}

func TestErrNotFoundIsAPlainNotFound(t *testing.T) {
	var terr *terrors.Error
	if err := errNotFound(); !errors.As(err, &terr) || !terr.PrefixMatches(terrors.ErrNotFound) || terr.Code != "not_found.account" {
		t.Fatalf("errNotFound() = %v, want not_found.account", err)
	}
	if len(terr.StackFrames) != 0 {
		t.Error("errNotFound shouldn't pay for a stack trace: a missing account is an expected answer")
	}
	// A new value each time: changing one can't change the next.
	terr.Message = "changed"
	if errors.As(errNotFound(), &terr); terr.Message != "account not found" {
		t.Error("errNotFound must return a new value each time, so nothing can change a shared one")
	}
}
