# 4. Represent money as int64 minor units plus a currency

- **Status:** Accepted
- **Date:** 2026-11-12

## Context

Almost every tiny-bank service will handle amounts: accounts hold balances, the ledger records postings, and transactions move money between customers. A wrong amount is the most serious bug a bank can have. The ways amounts usually go wrong are:

- **Rounding.** Binary floating point can't hold most decimal amounts exactly: `0.10 + 0.20` is `0.30000000000000004`. The error is tiny, but it adds up across many operations, and two systems can disagree by a penny.
- **Mixing currencies.** Adding £10 to €10 as if both were plain numbers gives 20 of nothing.
- **Overflow.** A fixed-size integer that goes past its maximum silently wraps around to a large negative number.
- **Typos.** A free-text currency code like `"GPB"` becomes a new, unknown currency.

Every service needs the same answer to these, so it belongs in one shared package, not repeated in each service.

## Decision

All amounts are `money.Money` from [`pkg/money`](../../pkg/money/money.go):

- **The amount is an `int64` of the currency's minor unit**, such as pence for GBP or cents for EUR. `money.New(1050, money.GBP())` is £10.50. An `int64` holds up to about 92 quadrillion pounds, far beyond any real balance.
- **The currency travels with the amount.** `Currency` is an ISO 4217 currency with its number of minor-unit digits (2 for GBP, 0 for JPY).
- **Currencies come only from a supported list.** `GBP()`, `EUR()`, `USD()` and `JPY()`, or `ParseCurrency` for untrusted input. `Currency` has unexported fields, so code can't invent one. The functions return fixed values, so code can't redefine one either. The zero values `Currency{}` and `Money{}` are invalid.
- **Arithmetic returns an error, never a wrong answer.** `Add` and `Sub` return `ErrCurrencyMismatch`, `ErrOverflow` or `ErrInvalidCurrency`, wrapped with the amounts involved, for example `add GBP 1.00 + EUR 2.50: currency mismatch`. On error the result is the zero `Money`, so a half-computed value can't be used by mistake.
- **`Money` is a small value type.** Copy it freely, and compare it with `==`. Amounts in different currencies are never equal.
- **`String()` is for logs and errors only** (`GBP -0.05`). Formatting for customers, with symbols and locales, belongs at the edge of the system.

The package only has what's needed now: add, subtract and currency checks. Comparison, parsing decimal strings like `"10.50"`, and encoding for JSON or the database are added when a service first needs them.

### Alternatives considered

- **`float64`.** Native and convenient, but inexact, as above. Ruled out for money everywhere in tiny-bank.
- **An arbitrary-precision decimal type** (such as `shopspring/decimal` or `math/big`). Exact, but slower, allocates memory, and needs rounding rules for every operation. It's also another dependency, and it still doesn't stop currencies being mixed. Banking amounts are always whole minor units, so the extra precision buys nothing.
- **Plain `int64` with a separate currency field on each struct.** Exact and simple, but nothing stops code adding a GBP number to an EUR number. Every service would have to remember the checks.
- **A third-party money library.** These usually take free-text currency codes and either panic or silently allow mismatches. Our rules are short enough to own, and owning them lets us test exactly the guarantees we rely on.
- **A string-based `Currency` type with constants.** Constants can't be reassigned, but `Currency("GPB")` would still compile, so typos would only be caught later, when used. Unexported fields plus functions catch them earlier.

## Consequences

- Every money bug of the kinds above becomes either a compile error or an error value. **Callers must check that error before using the result.** This is a rule we follow, not something Go enforces. Lint (`errcheck`) catches a call whose error is ignored completely, like `a.Add(b)` on its own line. But Go allows `sum, _ := a.Add(b)`, and our lint settings don't flag errors assigned to `_`. Code review must catch that. On error the result is the zero `Money`, which has no currency, so any later `Add` or `Sub` with it fails instead of quietly producing a number.
- Code is a little longer: `a.Add(b)` returns `(Money, error)`, not just a number, so each step needs `if err != nil`.
- Amounts with fractions of a minor unit, such as interest calculated to a fraction of a penny, can't be represented directly. When interest arrives, we'll add an explicit rounding step with a recorded rule.
- Adding a currency means adding one function and one map entry, with its correct minor-unit digits.
- Databases and APIs will store the amount as a 64-bit integer plus a three-letter code. That's the usual shape, and it's simple to index and query.
