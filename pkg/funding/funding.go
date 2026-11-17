// Package funding names the ledger's funding accounts: one per currency,
// where money that comes into the bank from outside is taken from. A deposit
// is a journal from the funding account to a customer's account, so the
// funding account's balance goes negative as money arrives.
//
// The ledger creates them (services/ledger/migrations) and treats them
// specially; the transaction service posts deposits from them. The IDs are
// fixed, and each ends in the currency's ISO 4217 number (826 for GBP), so
// they're easy to spot in logs. They live here, not in either service, so
// neither imports the other.
package funding

import (
	"maps"
	"uuid"

	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

var accounts = map[string]uuid.UUID{
	"GBP": uuid.MustParse("00000000-0000-4000-8000-000000000826"),
	"EUR": uuid.MustParse("00000000-0000-4000-8000-000000000978"),
	"USD": uuid.MustParse("00000000-0000-4000-8000-000000000840"),
	"JPY": uuid.MustParse("00000000-0000-4000-8000-000000000392"),
}

// Account returns the funding account for c, and false for a currency with none.
func Account(c money.Currency) (uuid.UUID, bool) {
	id, ok := accounts[c.Code()]
	return id, ok
}

// Is reports whether id is a funding account.
func Is(id uuid.UUID) bool {
	for _, f := range accounts {
		if f == id {
			return true
		}
	}
	return false
}

// All returns every funding account, by currency code. It's a copy, so
// changing it changes nothing.
func All() map[string]uuid.UUID { return maps.Clone(accounts) }
