//go:build integration

package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// newTestStore starts a throwaway Postgres, applies the service's real
// migrations, and returns a Store on it.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	// Start Postgres first: pulling the image can be slow, and shouldn't use up
	// the deadline for connecting and migrating.
	url := dbtest.StartPostgres(t, "s3cret")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool, Migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(pool)
}

func TestStoreCreateThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	before := time.Now().Add(-time.Minute) // allow for clock differences with the container
	created, err := s.Create(ctx, NewRequest{CustomerID: customer, HolderName: "  Ada Lovelace ", Currency: money.GBP()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil() {
		t.Error("ID is not set, want one from Postgres")
	}
	if created.CustomerID != customer || created.HolderName != "Ada Lovelace" || created.Currency != money.GBP() || created.Status != StatusOpen {
		t.Errorf("created = %+v, want our customer, a trimmed name, GBP, open", created)
	}
	if created.CreatedAt.Before(before) {
		t.Errorf("CreatedAt = %v, want about now", created.CreatedAt)
	}

	got, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt round trip: got %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	got.CreatedAt = created.CreatedAt // compared above; time.Time locations can differ
	if got != created {
		t.Errorf("Get = %+v, want what Create returned: %+v", got, created)
	}
}

func TestStoreCreateGivesEachAccountItsOwnID(t *testing.T) {
	s := newTestStore(t)
	a, errA := s.Create(t.Context(), NewRequest{CustomerID: customer, HolderName: "Ada", Currency: money.GBP()})
	b, errB := s.Create(t.Context(), NewRequest{CustomerID: customer, HolderName: "Ada", Currency: money.GBP()})
	if err := errors.Join(errA, errB); err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Errorf("two accounts share ID %s", a.ID)
	}
}

func TestStoreCreateRejectsInvalidRequests(t *testing.T) {
	s := newTestStore(t)
	for _, req := range []NewRequest{
		{CustomerID: customer, HolderName: "", Currency: money.GBP()},
		{CustomerID: customer, HolderName: "Ada", Currency: money.Currency{}},
		{CustomerID: uuid.Nil(), HolderName: "Ada", Currency: money.GBP()},
	} {
		if _, err := s.Create(t.Context(), req); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(%+v) err = %v, want ErrInvalid", req, err)
		}
	}
}

func TestStoreGetMissing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get(t.Context(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(random ID) err = %v, want ErrNotFound", err)
	}
}

func TestStoreOneCustomerManyCurrencies(t *testing.T) {
	// A customer with money in several currencies has one account per currency.
	s := newTestStore(t)
	seen := map[uuid.UUID]bool{}
	for _, c := range []money.Currency{money.GBP(), money.EUR(), money.USD()} {
		acc, err := s.Create(t.Context(), NewRequest{CustomerID: customer, HolderName: "Ada", Currency: c})
		if err != nil {
			t.Fatalf("Create %v: %v", c, err)
		}
		if acc.CustomerID != customer || acc.Currency != c || seen[acc.ID] {
			t.Errorf("account %+v: want customer %s, currency %v and a new ID", acc, customer, c)
		}
		seen[acc.ID] = true
	}
}

func TestStoreAcceptsEveryNameValidateAccepts(t *testing.T) {
	// The table's CHECK is a safety net. It must never refuse a name Validate
	// allows, or the caller would get a database error instead of an account.
	s := newTestStore(t)
	for _, name := range []string{
		"Zoë 山田",
		"مهر\u200cنوش",      // Persian, with a zero-width non-joiner
		"Ada\u00a0Lovelace", // a non-breaking space inside the name
		strings.Repeat("é", 200),
	} {
		if _, err := s.Create(t.Context(), NewRequest{CustomerID: customer, HolderName: name, Currency: money.GBP()}); err != nil {
			t.Errorf("Create(%q): %v", name, err)
		}
	}
}

func TestStoreCreateErrorStillMatchesErrInvalid(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(t.Context(), NewRequest{CustomerID: customer, Currency: money.GBP()})
	var ve *ValidationError
	if !errors.Is(err, ErrInvalid) || !errors.As(err, &ve) || ve.Reason != "holder name is required" {
		t.Fatalf("err = %v, want a ValidationError with just the reason", err)
	}
}
