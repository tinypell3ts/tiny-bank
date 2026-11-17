//go:build integration

package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// newTestStore returns a Store on a throwaway database with the service's
// real migrations applied. Every test ends by checking the ledger still adds
// up.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(dbtest.Migrated(t, Migrations()))
	t.Cleanup(func() { checkInvariants(t, s) })
	return s
}

// postJournal validates req and posts it with the Store, without the
// Service's account checks: these tests are about the Store's own rules.
func postJournal(s *Store, ctx context.Context, req NewJournal) (Journal, bool, error) {
	req, err := req.Validate()
	if err != nil {
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	return s.postValid(ctx, req)
}

// checkInvariants checks the two rules double-entry bookkeeping promises:
// every currency's balances add up to zero (money only moves), and every
// balance equals the sum of its account's entries (the stored balance never
// drifts from the history).
func checkInvariants(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background() // t.Context() is already cancelled during cleanup
	var unbalanced, drifted int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM (SELECT currency FROM accounts GROUP BY currency HAVING sum(balance) <> 0) c`,
	).Scan(&unbalanced); err != nil {
		t.Fatalf("check currencies: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts a
		 LEFT JOIN (SELECT account_id, sum(amount) AS total FROM entries GROUP BY account_id) e ON e.account_id = a.id
		 WHERE a.balance <> coalesce(e.total, 0)`,
	).Scan(&drifted); err != nil {
		t.Fatalf("check balances: %v", err)
	}
	if unbalanced != 0 || drifted != 0 {
		t.Errorf("ledger doesn't add up: %d currencies don't sum to zero, %d balances differ from their entries", unbalanced, drifted)
	}
}

func gbpFunding(t *testing.T) uuid.UUID {
	t.Helper()
	id, ok := FundingAccount(money.GBP())
	if !ok {
		t.Fatal("no GBP funding account")
	}
	return id
}

// fund moves minor pence into account from outside the bank.
func fund(t *testing.T, s *Store, account uuid.UUID, minor int64) {
	t.Helper()
	if _, _, err := postJournal(s, t.Context(), NewJournal{
		IdempotencyKey: "fund-" + uuid.New().String(),
		Entries:        []Entry{{gbpFunding(t), gbp(-minor)}, {account, gbp(minor)}},
	}); err != nil {
		t.Fatalf("fund %s: %v", account, err)
	}
}

func transfer(key string, from, to uuid.UUID, minor int64) NewJournal {
	return NewJournal{IdempotencyKey: key, Entries: []Entry{{from, gbp(-minor)}, {to, gbp(minor)}}}
}

func wantBalance(t *testing.T, s *Store, account uuid.UUID, want money.Money) {
	t.Helper()
	b, err := s.Balance(t.Context(), account)
	if err != nil {
		t.Fatalf("balance of %s: %v", account, err)
	}
	if b.Amount != want {
		t.Errorf("balance of %s = %s, want %s", account, b.Amount, want)
	}
}

// hasCode reports whether err is, or wraps, a terror with code. Store errors
// are wrapped with fmt.Errorf, which terrors.PrefixMatches doesn't look inside.
func hasCode(err error, code string) bool {
	var terr *terrors.Error
	return errors.As(err, &terr) && terr.Code == code
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !hasCode(err, code) {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func TestFundingAccountsAreSeeded(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []money.Currency{money.GBP(), money.EUR(), money.USD(), money.JPY()} {
		id, _ := FundingAccount(c)
		b, err := s.Balance(t.Context(), id)
		if err != nil {
			t.Fatalf("%s funding account: %v", c, err)
		}
		if b.Kind != KindFunding || b.Amount != money.New(0, c) {
			t.Errorf("%s funding account = %+v, want an empty funding account in %s", c, b, c)
		}
	}

	// And the other way round: every funding account the migrations create
	// is one FundingAccount knows, so none is unreachable from Go.
	rows, err := s.pool.Query(t.Context(), `SELECT id, currency FROM accounts WHERE kind = 'funding'`)
	if err != nil {
		t.Fatal(err)
	}
	type seeded struct {
		id       uuid.UUID
		currency string
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (seeded, error) {
		var f seeded
		return f, row.Scan(&f.id, &f.currency)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fundingAccounts) {
		t.Errorf("migrations create %d funding accounts, Go knows %d", len(got), len(fundingAccounts))
	}
	for _, f := range got {
		if want, ok := fundingAccounts[f.currency]; !ok || want != f.id {
			t.Errorf("funding account %s (%s) isn't the one FundingAccount returns for %s", f.id, f.currency, f.currency)
		}
	}
}

func TestPostMovesMoney(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)

	j, created, err := postJournal(s, t.Context(), transfer("t1", alice, bob, 300))
	if err != nil || !created {
		t.Fatalf("Post = %v, created %v; want a new journal", err, created)
	}
	wantBalance(t, s, alice, gbp(700))
	wantBalance(t, s, bob, gbp(300))
	wantBalance(t, s, gbpFunding(t), gbp(-1000))

	got, err := s.Journal(t.Context(), j.ID)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	if got.IdempotencyKey != "t1" || got.Currency != money.GBP() || !got.CreatedAt.Equal(j.CreatedAt) ||
		len(got.Entries) != 2 || got.Entries[0] != j.Entries[0] || got.Entries[1] != j.Entries[1] {
		t.Errorf("Journal = %+v, want what Post returned: %+v", got, j)
	}
}

func TestPostRefusesAnOverdraft(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 100)

	// aaron sorts before alice, so his credit is worked out before alice's
	// debit is refused. All of it must be undone, down to his new account row.
	aaron := uuid.MustParse("0aa00000-0000-4000-8000-000000000004")
	_, _, err := postJournal(s, t.Context(), transfer("t1", alice, aaron, 101))
	wantCode(t, err, "precondition_failed.insufficient_funds")
	wantBalance(t, s, alice, gbp(100))
	_, err = s.Balance(t.Context(), aaron)
	wantCode(t, err, "not_found.account")

	// A refused post doesn't use up its key, so once there's money the same
	// request can go through.
	fund(t, s, alice, 1)
	if _, created, err := postJournal(s, t.Context(), transfer("t1", alice, aaron, 101)); err != nil || !created {
		t.Fatalf("retry after funding = %v, created %v; want it posted", err, created)
	}
	wantBalance(t, s, alice, gbp(0))
}

func TestPostRefusesAnotherCurrency(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 100) // alice's account now holds GBP
	eurFunding, _ := FundingAccount(money.EUR())

	_, _, err := postJournal(s, t.Context(), NewJournal{IdempotencyKey: "e1", Entries: []Entry{
		{eurFunding, money.New(-100, money.EUR())}, {alice, money.New(100, money.EUR())},
	}})
	wantCode(t, err, "bad_request.currency")
}

func TestPostRefusesInvalidJournals(t *testing.T) {
	s := newTestStore(t)
	_, _, err := postJournal(s, t.Context(), NewJournal{IdempotencyKey: "k1", Entries: []Entry{{alice, gbp(1)}, {bob, gbp(1)}}})
	wantCode(t, err, "bad_request.entries")
}

func TestPostIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)
	first, _, err := postJournal(s, t.Context(), transfer("t1", alice, bob, 300))
	if err != nil {
		t.Fatal(err)
	}

	// The same request again, as after a timeout: the first journal, and no
	// money moves.
	again, created, err := postJournal(s, t.Context(), transfer("t1", alice, bob, 300))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry = %v, created %v, ID %s; want the first journal %s", err, created, again.ID, first.ID)
	}
	wantBalance(t, s, alice, gbp(700))

	// Entries in another order are still the same request.
	reordered := NewJournal{IdempotencyKey: "t1", Entries: []Entry{{bob, gbp(300)}, {alice, gbp(-300)}}}
	if again, created, err := postJournal(s, t.Context(), reordered); err != nil || created || again.ID != first.ID {
		t.Fatalf("reordered retry = %v, created %v; want the first journal", err, created)
	}

	// The same key for a different request is a caller bug.
	_, _, err = postJournal(s, t.Context(), transfer("t1", alice, bob, 301))
	wantCode(t, err, "precondition_failed.idempotency_key")
	wantBalance(t, s, alice, gbp(700))
}

func TestNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Balance(t.Context(), uuid.New())
	wantCode(t, err, "not_found.account")
	_, err = s.Journal(t.Context(), uuid.New())
	wantCode(t, err, "not_found.journal")
}

// runConcurrently runs post(i) for i in [0, n) at the same time, and returns
// the errors in order.
func runConcurrently(n int, post func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start // release them all at once, so they really do overlap
			errs[i] = post(i)
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// TestConcurrentPostsNeverOverdraw is the race the locks exist for. Without
// FOR UPDATE, two posts could both read alice's balance of 30, both decide
// there's enough, and both take 30.
func TestConcurrentPostsNeverOverdraw(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)

	errs := runConcurrently(50, func(i int) error {
		_, _, err := postJournal(s, t.Context(), transfer(fmt.Sprintf("t%d", i), alice, bob, 30))
		return err
	})
	var posted, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			posted++
		case hasCode(err, "precondition_failed.insufficient_funds"):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	// 1000 / 30 = 33 transfers fit, with 10 left over.
	if posted != 33 || refused != 17 {
		t.Errorf("%d posted, %d refused; want 33 and 17", posted, refused)
	}
	wantBalance(t, s, alice, gbp(10))
	wantBalance(t, s, bob, gbp(990))
}

// TestOppositeTransfersDontDeadlock sends money both ways between the same
// two accounts at once. If posts locked accounts in entry order instead of
// ID order, alice-to-bob would lock alice then wait for bob while
// bob-to-alice locked bob then waited for alice, and Postgres would abort one.
func TestOppositeTransfersDontDeadlock(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)
	fund(t, s, bob, 1000)

	errs := runConcurrently(50, func(i int) error {
		from, to := alice, bob
		if i%2 == 1 {
			from, to = bob, alice
		}
		_, _, err := postJournal(s, t.Context(), transfer(fmt.Sprintf("t%d", i), from, to, 10))
		return err
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("want every transfer posted: %v", err)
	}
	wantBalance(t, s, alice, gbp(1000))
	wantBalance(t, s, bob, gbp(1000))
}

// TestConcurrentRetriesPostOnce sends the same request many times at once, as
// a caller retrying impatiently might.
func TestConcurrentRetriesPostOnce(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)

	ids := make([]uuid.UUID, 20)
	var mu sync.Mutex
	var created int
	errs := runConcurrently(len(ids), func(i int) error {
		j, c, err := postJournal(s, t.Context(), transfer("same-key", alice, bob, 300))
		ids[i] = j.ID
		if c {
			mu.Lock()
			created++
			mu.Unlock()
		}
		return err
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("want every retry answered: %v", err)
	}
	if created != 1 {
		t.Errorf("%d posts created a journal, want exactly 1", created)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("retries got different journals: %v", ids)
		}
	}
	wantBalance(t, s, alice, gbp(700))
}

// TestJournalsAndEntriesAreAppendOnly checks the audit trail: once written,
// history can't be changed or removed, even by SQL that skips the Go code.
func TestJournalsAndEntriesAreAppendOnly(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)

	for _, sql := range []string{
		`UPDATE entries SET amount = amount * 2`,
		`DELETE FROM entries`,
		`TRUNCATE entries`,
		`UPDATE journals SET currency = 'EUR'`,
		`DELETE FROM journals`,
		`TRUNCATE journals CASCADE`,
	} {
		if _, err := s.pool.Exec(t.Context(), sql); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want it refused as append-only", sql, err)
		}
	}
	wantBalance(t, s, alice, gbp(1000)) // and the balance still matches its entries (checkInvariants)
}
