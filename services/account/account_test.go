package account

import (
	"errors"
	"strings"
	"testing"
	"uuid"

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
				var ve *ValidationError
				if !errors.Is(err, ErrInvalid) || !errors.As(err, &ve) || !strings.Contains(ve.Reason, tt.wantErr) {
					t.Fatalf("err = %v, want a ValidationError whose Reason contains %q", err, tt.wantErr)
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
