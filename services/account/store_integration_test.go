//go:build integration

package account

import (
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// newTestStore returns a Store on a throwaway database with the service's
// real migrations applied.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(dbtest.Migrated(t, Migrations()))
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
		// terrors.PrefixMatches(err, …) doesn't look inside wrapping, and Create
		// wraps ("create account: …"), so find the terror with errors.As.
		var terr *terrors.Error
		if _, err := s.Create(t.Context(), req); !errors.As(err, &terr) || !terr.PrefixMatches(terrors.ErrBadRequest) {
			t.Errorf("Create(%+v) err = %v, want a terrors bad_request", req, err)
		}
	}
}

func TestStoreGetMissing(t *testing.T) {
	s := newTestStore(t)
	var terr *terrors.Error
	if _, err := s.Get(t.Context(), uuid.New()); !errors.As(err, &terr) || !terr.PrefixMatches(terrors.ErrNotFound) {
		t.Errorf("Get(random ID) err = %v, want a terrors not_found", err)
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

func TestStoreCreateErrorKeepsItsTerror(t *testing.T) {
	// Create wraps Validate's error with context ("create account: ..."); the
	// terror inside must still be found, so kit.Handler sends its code.
	s := newTestStore(t)
	_, err := s.Create(t.Context(), NewRequest{CustomerID: customer, Currency: money.GBP()})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "bad_request.holder_name" || terr.Message != "holder name is required" {
		t.Fatalf("err = %v, want bad_request.holder_name with just the reason", err)
	}
}

// TestAccountsAreReadByName: id and customer_id are both UUIDs, so read by
// position, swapping them in a query would hand back the customer's ID as
// the account's, with no error. Read by name, the order doesn't matter.
func TestAccountsAreReadByName(t *testing.T) {
	s := newTestStore(t)
	want, err := s.Create(t.Context(), NewRequest{CustomerID: uuid.New(), HolderName: "Ada", Currency: money.GBP()})
	if err != nil {
		t.Fatal(err)
	}
	swapped := strings.Replace(columns, "id, customer_id", "customer_id, id", 1)
	if swapped == columns {
		t.Fatal("test is stale: columns no longer starts id, customer_id")
	}
	got, err := s.queryAccount(t.Context(), "SELECT "+swapped+" FROM accounts WHERE id = $1", want.ID)
	if err != nil || got.ID != want.ID || got.CustomerID != want.CustomerID {
		t.Errorf("with the columns swapped: id %s, customer %s, %v; want %s, %s", got.ID, got.CustomerID, err, want.ID, want.CustomerID)
	}
}
