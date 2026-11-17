package funding

import (
	"testing"
	"uuid"

	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

func TestEverySupportedCurrencyHasOne(t *testing.T) {
	for _, c := range []money.Currency{money.GBP(), money.EUR(), money.USD(), money.JPY()} {
		id, ok := Account(c)
		if !ok || !Is(id) {
			t.Errorf("%s: %s, %v; want a funding account", c, id, ok)
		}
	}
	if _, ok := Account(money.Currency{}); ok {
		t.Error("the zero currency has a funding account")
	}
	if Is(uuid.New()) {
		t.Error("a random ID is a funding account")
	}
}

func TestAllIsACopy(t *testing.T) {
	all := All()
	delete(all, "GBP")
	if _, ok := Account(money.GBP()); !ok {
		t.Error("changing All's result changed the funding accounts")
	}
}
