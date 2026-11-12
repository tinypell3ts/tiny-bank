package money

import (
	"errors"
	"fmt"
)

// ErrInvalidCurrency means a currency code isn't one tiny-bank supports, or a
// Currency or Money was used without being set (its zero value).
var ErrInvalidCurrency = errors.New("invalid currency")

// Currency is an ISO 4217 currency, such as GBP.
//
// Its fields are unexported, so the only way to get one is ParseCurrency or the
// variables below. A typo like "GPB" fails when it is parsed instead of
// turning into a new currency. The zero value Currency{} is invalid.
type Currency struct {
	code     string
	exponent int // digits after the decimal point: 2 for GBP (pence), 0 for JPY
}

// Supported currencies. Add one here only when tiny-bank needs it.
var (
	GBP = Currency{code: "GBP", exponent: 2}
	EUR = Currency{code: "EUR", exponent: 2}
	USD = Currency{code: "USD", exponent: 2}
	JPY = Currency{code: "JPY", exponent: 0} // the yen has no minor unit
)

var currencies = map[string]Currency{
	GBP.code: GBP,
	EUR.code: EUR,
	USD.code: USD,
	JPY.code: JPY,
}

// ParseCurrency returns the supported currency for an ISO 4217 code. The code
// must be exactly three upper-case letters, like "GBP".
func ParseCurrency(code string) (Currency, error) {
	c, ok := currencies[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	return c, nil
}

// Code returns the ISO 4217 code, like "GBP".
func (c Currency) Code() string { return c.code }

// Exponent returns how many digits the minor unit has: 2 for GBP, where
// 100 pence make a pound, and 0 for JPY.
func (c Currency) Exponent() int { return c.exponent }

// String returns the ISO 4217 code, or "???" for the zero value.
func (c Currency) String() string {
	if !c.valid() {
		return "???"
	}
	return c.code
}

func (c Currency) valid() bool { return c.code != "" }
