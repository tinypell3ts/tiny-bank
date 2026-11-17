//go:build integration

package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// fakeAccounts answers account lookups from a map, as the account service
// would, and records which accounts were asked about. An account missing from
// the map doesn't exist. Set err to make every lookup fail.
type fakeAccounts struct {
	mu    sync.Mutex
	info  map[uuid.UUID]AccountInfo
	asked []uuid.UUID
	err   error
}

func (f *fakeAccounts) Account(_ context.Context, id uuid.UUID) (AccountInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	if f.err != nil {
		return AccountInfo{}, f.err
	}
	info, ok := f.info[id]
	if !ok {
		return AccountInfo{}, errNoSuchAccount(id)
	}
	return info, nil
}

func openGBP() AccountInfo { return AccountInfo{Open: true, Currency: money.GBP()} }

func newTestService(t *testing.T) (*Service, *fakeAccounts) {
	t.Helper()
	accounts := &fakeAccounts{info: map[uuid.UUID]AccountInfo{alice: openGBP(), bob: openGBP()}}
	return NewService(newTestStore(t), accounts, prometheus.NewRegistry()), accounts
}

func TestServicePostsBetweenOpenAccounts(t *testing.T) {
	s, accounts := newTestService(t)
	fund(t, s.Store, alice, 1000)

	if _, created, err := s.Post(t.Context(), transfer("t1", alice, bob, 300)); err != nil || !created {
		t.Fatalf("Post = %v, created %v; want it posted", err, created)
	}
	wantBalance(t, s.Store, bob, gbp(300))
	if len(accounts.asked) != 2 {
		t.Errorf("asked about %v, want alice and bob once each", accounts.asked)
	}
}

func TestServiceDoesntAskAboutFundingAccounts(t *testing.T) {
	s, accounts := newTestService(t)
	if _, _, err := s.Post(t.Context(), transfer("deposit", gbpFunding(t), alice, 500)); err != nil {
		t.Fatal(err)
	}
	if len(accounts.asked) != 1 || accounts.asked[0] != alice {
		t.Errorf("asked about %v, want only alice: funding accounts are the ledger's own", accounts.asked)
	}
}

func TestServiceRefusesAccountsItCantUse(t *testing.T) {
	stranger := uuid.MustParse("5c0ff000-0000-4000-8000-000000000009")
	tests := []struct {
		name     string
		set      func(*fakeAccounts)
		to       uuid.UUID
		wantCode string
	}{
		{"doesn't exist (a typo, say)", func(*fakeAccounts) {}, stranger, "not_found.account"},
		{"isn't open", func(f *fakeAccounts) { f.info[bob] = AccountInfo{Currency: money.GBP()} }, bob, "precondition_failed.account_not_open"},
		{"holds another currency", func(f *fakeAccounts) { f.info[bob] = AccountInfo{Open: true, Currency: money.EUR()} }, bob, "bad_request.currency"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, accounts := newTestService(t)
			fund(t, s.Store, alice, 1000)
			tt.set(accounts)

			_, _, err := s.Post(t.Context(), transfer("t1", alice, tt.to, 300))
			wantCode(t, err, tt.wantCode)
			wantBalance(t, s.Store, alice, gbp(1000)) // nothing moved
		})
	}
}

// TestServiceKeepsAnAccountCheckRefusal: a request refused because an
// account couldn't be used stays refused once the account can be, so a late
// repeat with the same key never moves money its caller was told didn't.
func TestServiceKeepsAnAccountCheckRefusal(t *testing.T) {
	s, accounts := newTestService(t)
	fund(t, s.Store, alice, 1000)
	accounts.info[bob] = AccountInfo{Currency: money.GBP()} // not open yet
	_, _, err := s.Post(t.Context(), transfer("t1", alice, bob, 300))
	wantCode(t, err, "precondition_failed.account_not_open")

	accounts.info[bob] = openGBP()
	accounts.asked = nil
	_, created, err := s.Post(t.Context(), transfer("t1", alice, bob, 300))
	wantCode(t, err, "precondition_failed.account_not_open")
	if created || len(accounts.asked) != 0 {
		t.Errorf("repeat created %v after asking about %v; want the kept refusal, with no checks", created, accounts.asked)
	}
	wantBalance(t, s.Store, alice, gbp(1000))
}

// TestServiceDoesntKeepAnUnsureAnswer: when the account service can't be
// reached, nothing is decided, so the same request can go through later.
func TestServiceDoesntKeepAnUnsureAnswer(t *testing.T) {
	s, accounts := newTestService(t)
	fund(t, s.Store, alice, 1000)
	accounts.err = errors.New("account service unreachable")
	if _, _, err := s.Post(t.Context(), transfer("t1", alice, bob, 300)); err == nil {
		t.Fatal("posted without being able to check the accounts")
	}
	accounts.err = nil
	if _, created, err := s.Post(t.Context(), transfer("t1", alice, bob, 300)); err != nil || !created {
		t.Fatalf("retry once the account service is back = %v, created %v; want it posted", err, created)
	}
	wantBalance(t, s.Store, alice, gbp(700))
}

func TestServiceFailsClosedWhenTheAccountServiceDoes(t *testing.T) {
	s, accounts := newTestService(t)
	fund(t, s.Store, alice, 1000)
	accounts.err = errors.New("account service unreachable")

	if _, _, err := s.Post(t.Context(), transfer("t1", alice, bob, 300)); err == nil {
		t.Fatal("posted without being able to check the accounts")
	}
	wantBalance(t, s.Store, alice, gbp(1000))
}

// TestServiceRetryGetsTheFirstJournal: once money has moved, a retry must say
// so, even if the account can't be used any more. Refusing it would tell the
// caller the money hadn't moved.
func TestServiceRetryGetsTheFirstJournal(t *testing.T) {
	s, accounts := newTestService(t)
	fund(t, s.Store, alice, 1000)
	first, _, err := s.Post(t.Context(), transfer("t1", alice, bob, 300))
	if err != nil {
		t.Fatal(err)
	}

	accounts.info[bob] = AccountInfo{Currency: money.GBP()} // bob's account is no longer open
	accounts.asked = nil
	again, created, err := s.Post(t.Context(), transfer("t1", alice, bob, 300))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry = %v, created %v; want the first journal", err, created)
	}
	if len(accounts.asked) != 0 {
		t.Errorf("a retry asked about %v, want no checks", accounts.asked)
	}

	// The same key for a different request is still refused.
	_, _, err = s.Post(t.Context(), transfer("t1", alice, bob, 301))
	wantCode(t, err, "precondition_failed.idempotency_key")
}

// gatedAccounts holds every lookup until gate is closed, then fails it, like
// an account service that times out.
type gatedAccounts struct {
	asked chan struct{} // gets a value when a lookup starts
	gate  chan struct{}
}

func (g gatedAccounts) Account(_ context.Context, _ uuid.UUID) (AccountInfo, error) {
	g.asked <- struct{}{}
	<-g.gate
	return AccountInfo{}, errors.New("account service timed out")
}

// TestServiceRetryOverlappingTheFirstPostGetsItsJournal is Greptile's race on
// PR #10: two requests with the same key both find no journal yet. The first
// passes its checks and posts; the second's account lookup times out. The
// second must still get the first's journal, since its money has moved.
func TestServiceRetryOverlappingTheFirstPostGetsItsJournal(t *testing.T) {
	first, _ := newTestService(t)
	fund(t, first.Store, alice, 1000)
	slow := gatedAccounts{asked: make(chan struct{}, 2), gate: make(chan struct{})}
	retry := NewService(first.Store, slow, prometheus.NewRegistry())

	type result struct {
		j       Journal
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		j, created, err := retry.Post(t.Context(), transfer("t1", alice, bob, 300))
		done <- result{j, created, err}
	}()
	<-slow.asked // the retry has missed the lookup and is checking accounts

	posted, created, err := first.Post(t.Context(), transfer("t1", alice, bob, 300))
	if err != nil || !created {
		t.Fatalf("first post = %v, created %v", err, created)
	}
	close(slow.gate) // now the retry's account lookup fails

	got := <-done
	if got.err != nil || got.created || got.j.ID != posted.ID {
		t.Fatalf("retry = %v, created %v; want the first post's journal %s, not an error", got.err, got.created, posted.ID)
	}
	wantBalance(t, first.Store, alice, gbp(700)) // moved once
}
