// Package money represents amounts of money safely.
//
// An amount is a whole number of the currency's minor unit (pence for GBP,
// cents for EUR) in an int64, plus its currency. Never a float: floats can't
// hold most decimal amounts exactly (0.1 + 0.2 != 0.3), and the errors add up.
//
// Arithmetic never mixes currencies and never overflows silently: it returns
// an error instead.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrCurrencyMismatch means two amounts in different currencies were combined.
	ErrCurrencyMismatch = errors.New("currency mismatch")
	// ErrOverflow means the result doesn't fit in an int64 of minor units.
	ErrOverflow = errors.New("amount overflow")
)

// Money is an amount in one currency. It's a small value type: copy it freely
// and compare it with ==.
//
// The zero value Money{} has no currency and is invalid: Add and Sub return
// ErrInvalidCurrency for it.
type Money struct {
	amount   int64 // in minor units, e.g. pence
	currency Currency
}

// New returns an amount of minor units in currency c. money.New(1050, money.GBP())
// is £10.50.
func New(minor int64, c Currency) Money {
	return Money{amount: minor, currency: c}
}

// Amount returns the amount in minor units, e.g. 1050 for £10.50.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the amount's currency.
func (m Money) Currency() Currency { return m.currency }

// Add returns m + o. Both must be in the same, valid currency.
func (m Money) Add(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, fmt.Errorf("add %s + %s: %w", m, o, err)
	}
	sum := m.amount + o.amount
	// Signed overflow wraps around, so the sum moves the wrong way.
	if (o.amount > 0 && sum < m.amount) || (o.amount < 0 && sum > m.amount) {
		return Money{}, fmt.Errorf("add %s + %s: %w", m, o, ErrOverflow)
	}
	return Money{amount: sum, currency: m.currency}, nil
}

// Sub returns m - o. Both must be in the same, valid currency.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.sameCurrency(o); err != nil {
		return Money{}, fmt.Errorf("sub %s - %s: %w", m, o, err)
	}
	diff := m.amount - o.amount
	if (o.amount > 0 && diff > m.amount) || (o.amount < 0 && diff < m.amount) {
		return Money{}, fmt.Errorf("sub %s - %s: %w", m, o, ErrOverflow)
	}
	return Money{amount: diff, currency: m.currency}, nil
}

func (m Money) sameCurrency(o Money) error {
	if !m.currency.valid() || !o.currency.valid() {
		return ErrInvalidCurrency
	}
	if m.currency != o.currency {
		return ErrCurrencyMismatch
	}
	return nil
}

// String formats the amount for logs and errors, like "GBP 10.50",
// "GBP -0.05" or "JPY 500". It is not meant for showing to customers.
func (m Money) String() string {
	// Work on the absolute value as a uint64, because -math.MinInt64 doesn't fit
	// in an int64. Converting a negative int64 wraps around on purpose, and
	// negating it as a uint64 gives the right size (see TestString).
	neg := m.amount < 0
	abs := uint64(m.amount) //nolint:gosec // deliberate wrap-around, see above
	if neg {
		abs = -abs
	}

	digits := strconv.FormatUint(abs, 10)
	if exp := m.currency.exponent; exp > 0 {
		if len(digits) <= exp { // pad 5 -> "005" so it becomes "0.05"
			digits = strings.Repeat("0", exp-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-exp] + "." + digits[len(digits)-exp:]
	}
	if neg {
		digits = "-" + digits
	}
	return m.currency.String() + " " + digits
}
