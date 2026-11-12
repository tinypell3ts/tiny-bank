# Phase 3 — `pkg/money`

## What we built

- **`money.Currency`** ([currency.go](../../pkg/money/currency.go)): an ISO 4217 currency with its number of minor-unit digits. The supported ones are `GBP()`, `EUR()`, `USD()` and `JPY()`, and `ParseCurrency("GBP")` handles untrusted input. Nothing else can create or redefine a currency, so a typo like `"GPB"` is rejected.
- **`money.Money`** ([money.go](../../pkg/money/money.go)): an `int64` amount in minor units plus its currency. `Add` and `Sub` return `ErrCurrencyMismatch`, `ErrOverflow` or `ErrInvalidCurrency` instead of a wrong answer. `String()` formats amounts for logs (`GBP 10.50`).
- **Tests** ([money_test.go](../../pkg/money/money_test.go)): table tests for every rule, including the exact `int64` limits, and a fuzz test that checks `(a + b) - b == a`. Coverage is 100%.
- **Decision:** [ADR-0004](../adr/0004-represent-money-as-int64-minor-units.md).

## Concepts

- **Why not floats.** A `float64` stores numbers in binary, and most decimals, like 0.1, have no exact binary form. So `0.1 + 0.2` is `0.30000000000000004`. Whole pence in an `int64` are always exact.
- **Minor units and exponents.** The minor unit is the smallest coin: 1 penny is £0.01, so GBP has an exponent of 2. The yen has no smaller unit, so JPY's exponent is 0. The amount is always a whole number of minor units.
- **Signed overflow.** An `int64` runs from about -9.2 × 10¹⁸ to 9.2 × 10¹⁸. Going past the top wraps around to the bottom. We detect it by checking whether the result moved the wrong way: adding a positive number must never make it smaller.
- **Go errors are values.** Functions return `(result, error)`, and callers check `if err != nil`. Sentinel errors (`var ErrOverflow = errors.New(...)`) are wrapped with context using `fmt.Errorf("...: %w", err)`. Callers test them with `errors.Is`, even through the wrapping.
- **Unexported fields as a guarantee.** A struct whose fields start with a lower-case letter can only be built inside its own package. Code elsewhere has to use `ParseCurrency` or the currency functions, so every `Currency` is valid or empty.
- **No struct constants in Go.** `const` only works for numbers, strings and booleans, and an exported `var` can be reassigned by any package. A function that returns a fixed value is the usual way to share one nobody can change.
- **Value types and `==`.** Small structs are copied when passed around, and two structs are equal when all their fields are equal. So `New(1050, GBP()) == New(1050, GBP())`, and a copy can't be changed behind your back.
- **Zero values.** Every Go type has a zero value (`Money{}`), which exists without being set up. We make the zero value invalid and reject it, rather than let it quietly mean "0 of nothing".
- **Fuzz testing.** `go test` runs the fuzz test's seed inputs every time. `go test -fuzz` generates millions of random inputs and saves any that fail, so they become permanent test cases.

## How to verify

```bash
make check
```

Look for `pkg/money ... coverage: 100.0% of statements`.

```bash
go test -v -run 'TestAdd|TestSub' ./pkg/money
```

This lists every case, including `mixed currencies` and `overflow`.

```bash
go test -run '^$' -fuzz=FuzzAddThenSub -fuzztime=30s ./pkg/money
```

It should end with `PASS`.
