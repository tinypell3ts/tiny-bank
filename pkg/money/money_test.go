package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		code    string
		want    Currency
		wantErr bool
	}{
		{code: "GBP", want: GBP},
		{code: "EUR", want: EUR},
		{code: "USD", want: USD},
		{code: "JPY", want: JPY},
		{code: "GPB", wantErr: true}, // typo
		{code: "gbp", wantErr: true}, // ISO 4217 codes are upper case
		{code: "GBP ", wantErr: true},
		{code: "", wantErr: true},
		{code: "XYZ", wantErr: true}, // not supported
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			got, err := ParseCurrency(tt.code)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidCurrency) {
					t.Fatalf("err = %v, want ErrInvalidCurrency", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseCurrency(%q) = %v, %v; want %v", tt.code, got, err, tt.want)
			}
		})
	}
}

func TestCurrencyAccessors(t *testing.T) {
	if GBP.Code() != "GBP" || GBP.Exponent() != 2 || GBP.String() != "GBP" {
		t.Errorf("GBP = %q, %d, %q", GBP.Code(), GBP.Exponent(), GBP.String())
	}
	if JPY.Exponent() != 0 {
		t.Errorf("JPY exponent = %d, want 0", JPY.Exponent())
	}
	if got := (Currency{}).String(); got != "???" {
		t.Errorf("zero Currency String() = %q, want ???", got)
	}
}

func TestNewAndAccessors(t *testing.T) {
	m := New(1050, GBP)
	if m.Amount() != 1050 || m.Currency() != GBP {
		t.Fatalf("New(1050, GBP) = %d %v", m.Amount(), m.Currency())
	}
	a, b := New(1050, GBP), New(1050, GBP)
	if a != b {
		t.Error("equal amounts should compare equal with ==")
	}
	if a == New(1050, EUR) {
		t.Error("the same number in different currencies must not be equal")
	}
}

func TestAdd(t *testing.T) {
	tests := []struct {
		name    string
		a, b    Money
		want    Money
		wantErr error
	}{
		{name: "simple", a: New(1050, GBP), b: New(250, GBP), want: New(1300, GBP)},
		{name: "negative", a: New(100, GBP), b: New(-250, GBP), want: New(-150, GBP)},
		{name: "zero", a: New(0, EUR), b: New(0, EUR), want: New(0, EUR)},
		{name: "largest allowed", a: New(math.MaxInt64-1, GBP), b: New(1, GBP), want: New(math.MaxInt64, GBP)},
		{name: "smallest allowed", a: New(math.MinInt64+1, GBP), b: New(-1, GBP), want: New(math.MinInt64, GBP)},
		{name: "overflow", a: New(math.MaxInt64, GBP), b: New(1, GBP), wantErr: ErrOverflow},
		{name: "underflow", a: New(math.MinInt64, GBP), b: New(-1, GBP), wantErr: ErrOverflow},
		{name: "mixed currencies", a: New(100, GBP), b: New(100, EUR), wantErr: ErrCurrencyMismatch},
		{name: "zero Money on the left", a: Money{}, b: New(100, GBP), wantErr: ErrInvalidCurrency},
		{name: "zero Money on the right", a: New(100, GBP), b: Money{}, wantErr: ErrInvalidCurrency},
		{name: "both zero Money", a: Money{}, b: Money{}, wantErr: ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.a.Add(tt.b)
			check(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestSub(t *testing.T) {
	tests := []struct {
		name    string
		a, b    Money
		want    Money
		wantErr error
	}{
		{name: "simple", a: New(1050, GBP), b: New(250, GBP), want: New(800, GBP)},
		{name: "goes negative", a: New(100, GBP), b: New(250, GBP), want: New(-150, GBP)},
		{name: "minus a negative", a: New(100, GBP), b: New(-50, GBP), want: New(150, GBP)},
		{name: "largest allowed", a: New(math.MaxInt64-1, GBP), b: New(-1, GBP), want: New(math.MaxInt64, GBP)},
		{name: "smallest allowed", a: New(math.MinInt64+1, GBP), b: New(1, GBP), want: New(math.MinInt64, GBP)},
		{name: "overflow", a: New(math.MaxInt64, GBP), b: New(-1, GBP), wantErr: ErrOverflow},
		{name: "underflow", a: New(math.MinInt64, GBP), b: New(1, GBP), wantErr: ErrOverflow},
		{name: "zero minus the smallest overflows", a: New(0, GBP), b: New(math.MinInt64, GBP), wantErr: ErrOverflow},
		{name: "mixed currencies", a: New(100, GBP), b: New(100, USD), wantErr: ErrCurrencyMismatch},
		{name: "zero Money", a: Money{}, b: New(100, GBP), wantErr: ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.a.Sub(tt.b)
			check(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestErrorsSayWhatFailed(t *testing.T) {
	_, err := New(100, GBP).Add(New(250, EUR))
	if want := "add GBP 1.00 + EUR 2.50: currency mismatch"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		m    Money
		want string
	}{
		{New(1050, GBP), "GBP 10.50"},
		{New(5, GBP), "GBP 0.05"},
		{New(50, GBP), "GBP 0.50"},
		{New(100, GBP), "GBP 1.00"},
		{New(0, GBP), "GBP 0.00"},
		{New(-5, GBP), "GBP -0.05"},
		{New(-1050, EUR), "EUR -10.50"},
		{New(500, JPY), "JPY 500"},
		{New(-500, JPY), "JPY -500"},
		{New(math.MaxInt64, GBP), "GBP 92233720368547758.07"},
		{New(math.MinInt64, GBP), "GBP -92233720368547758.08"},
		{Money{}, "??? 0"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.m.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// FuzzAddThenSub checks that subtracting what you added gets you back where you
// started, for any amounts that don't overflow. `go test` runs the seeds below;
// `go test -fuzz=FuzzAddThenSub ./pkg/money` tries millions of random ones.
func FuzzAddThenSub(f *testing.F) {
	for _, seed := range [][2]int64{{0, 0}, {1050, 250}, {-1, 1}, {math.MaxInt64, -1}, {math.MinInt64, 1}, {math.MaxInt64, 1}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, a, b int64) {
		sum, err := New(a, GBP).Add(New(b, GBP))
		if err != nil {
			if !errors.Is(err, ErrOverflow) {
				t.Fatalf("Add(%d, %d): unexpected error %v", a, b, err)
			}
			return
		}
		back, err := sum.Sub(New(b, GBP))
		if err != nil || back != New(a, GBP) {
			t.Fatalf("(%d + %d) - %d = %v, %v; want %d", a, b, b, back, err, a)
		}
	})
}

func check(t *testing.T, got Money, err error, want Money, wantErr error) {
	t.Helper()
	if wantErr != nil {
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if got != (Money{}) {
			t.Errorf("on error got %v, want the zero Money", got)
		}
		return
	}
	if err != nil || got != want {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}
