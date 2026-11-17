package ledger

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
	accountv1 "github.com/tinypell3ts/tiny-bank/proto/account/v1"
)

// fakeAccountService serves GET /accounts/:id from accounts, behind
// kit.Handler as the real account service runs, and returns an
// AccountService pointing at it. An ID that isn't in accounts is a 404, and
// broken makes every lookup a 500 that leaks a detail.
func fakeAccountService(t *testing.T, accounts map[uuid.UUID]*accountv1.Account, broken bool) AccountService {
	t.Helper()
	router := typhon.Router{}
	router.GET("/accounts/:id", func(req typhon.Request) typhon.Response {
		if broken {
			return typhon.Response{Error: terrors.InternalService("db", "account database detail", nil)}
		}
		id, _ := kit.ParseUUID(typhon.RouterForRequest(req).Params(req)["id"])
		acc, ok := accounts[id]
		if !ok {
			return typhon.Response{Error: terrors.NotFound("account", "account not found", nil)}
		}
		return kit.Respond(req, acc, http.StatusOK)
	})
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, &ready, nil))
	t.Cleanup(srv.Close)
	return AccountService{BaseURL: srv.URL}
}

func TestAccountService(t *testing.T) {
	open := accountv1.AccountStatus_ACCOUNT_STATUS_OPEN
	accounts := fakeAccountService(t, map[uuid.UUID]*accountv1.Account{
		alice: {Id: alice.String(), Currency: "GBP", Status: open},
		bob:   {Id: bob.String(), Currency: "EUR"},                      // status not set
		carol: {Id: carol.String(), Currency: "GBP", Status: open + 41}, // a status this ledger doesn't know
	}, false)

	if got, err := accounts.Account(t.Context(), alice); err != nil || !got.Open || got.Currency.Code() != "GBP" {
		t.Errorf("alice = %+v, %v; want open, GBP", got, err)
	}
	// Unset and unknown statuses both mean "not open", as the contract says.
	for _, id := range []uuid.UUID{bob, carol} {
		if got, err := accounts.Account(t.Context(), id); err != nil || got.Open {
			t.Errorf("%s = %+v, %v; want it not open", id, got, err)
		}
	}

	_, err := accounts.Account(t.Context(), uuid.New())
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "not_found.account" || !strings.Contains(terr.Message, "doesn't exist") {
		t.Errorf("missing account: err = %v, want the ledger's own not_found.account", err)
	}
}

func TestAccountServiceHidesItsErrors(t *testing.T) {
	accounts := fakeAccountService(t, nil, true)
	_, err := accounts.Account(t.Context(), alice)
	if err == nil {
		t.Fatal("want an error when the account service fails")
	}
	// The terror the ledger's caller sees must be the ledger's own, not the
	// account service's internal_service.db with its detail.
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "internal_service.account_service" || strings.Contains(terr.Message, "detail") {
		t.Errorf("err = %v; want the ledger's own internal_service.account_service", err)
	}
}

func TestAccountServiceWhenItsDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := (AccountService{BaseURL: srv.URL}).Account(t.Context(), alice); err == nil {
		t.Fatal("want an error when the account service is down")
	}
}

// TestOnlyTheServiceCanBeTheLedger: the Store mustn't satisfy Ledger, or it
// could be wired into the routes on its own and post without account checks.
func TestOnlyTheServiceCanBeTheLedger(t *testing.T) {
	var _ Ledger = (*Service)(nil)
	if _, ok := any(&Store{}).(Ledger); ok {
		t.Error("*Store implements Ledger: it can be wired in without the Service's account checks")
	}
}

// slowAccounts answers every lookup after a delay, as a slow account service
// would. bad accounts don't exist.
type slowAccounts struct {
	delay time.Duration
	bad   map[uuid.UUID]bool
}

func (s slowAccounts) Account(ctx context.Context, id uuid.UUID) (AccountInfo, error) {
	time.Sleep(s.delay)
	if s.bad[id] {
		return AccountInfo{}, errNoSuchAccount(id)
	}
	return AccountInfo{Open: true, Currency: money.GBP()}, nil
}

func TestCheckAccountsAtOnce(t *testing.T) {
	entries := manyEntries(maxEntries) // ten customer accounts
	start := time.Now()
	if err := checkAccounts(t.Context(), slowAccounts{delay: 200 * time.Millisecond}, entries); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("checking %d accounts took %v; want about one lookup's time, not one per account", len(entries), took)
	}
}

func TestCheckAccountsReportsTheFirstFailureInOrder(t *testing.T) {
	j, err := NewJournal{IdempotencyKey: "k", Entries: []Entry{{alice, gbp(-550)}, {bob, gbp(500)}, {carol, gbp(50)}}}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	accounts := slowAccounts{bad: map[uuid.UUID]bool{bob: true, carol: true}}
	for range 20 { // goroutines finish in any order; the answer mustn't
		err := checkAccounts(t.Context(), accounts, j.Entries)
		var terr *terrors.Error
		if !errors.As(err, &terr) || !strings.Contains(terr.Message, bob.String()) {
			t.Fatalf("err = %v, want bob's, the first failing account in ID order", err)
		}
	}
}
