//go:build integration

package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/funding"
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
// drifts from the history). Both hold shard by shard: an account's own row
// is shard 0, and account_shards holds the rest.
func checkInvariants(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background() // t.Context() is already cancelled during cleanup
	const shards = `SELECT id AS account_id, 0::smallint AS shard, currency, balance FROM accounts
		UNION ALL SELECT s.account_id, s.shard, a.currency, s.balance FROM account_shards s JOIN accounts a ON a.id = s.account_id`
	var unbalanced, drifted int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM (SELECT currency FROM (`+shards+`) b GROUP BY currency HAVING sum(balance) <> 0) c`,
	).Scan(&unbalanced); err != nil {
		t.Fatalf("check currencies: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM (`+shards+`) b
		 LEFT JOIN (SELECT account_id, shard, sum(amount) AS total FROM entries GROUP BY account_id, shard) e
		   ON e.account_id = b.account_id AND e.shard = b.shard
		 WHERE b.balance <> coalesce(e.total, 0)`,
	).Scan(&drifted); err != nil {
		t.Fatalf("check balances: %v", err)
	}
	if unbalanced != 0 || drifted != 0 {
		t.Errorf("ledger doesn't add up: %d currencies don't sum to zero, %d balances differ from their entries", unbalanced, drifted)
	}
}

func gbpFunding(t *testing.T) uuid.UUID {
	t.Helper()
	id, ok := funding.Account(money.GBP())
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
		id, _ := funding.Account(c)
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
	if len(got) != len(funding.All()) {
		t.Errorf("migrations create %d funding accounts, Go knows %d", len(got), len(funding.All()))
	}
	for _, f := range got {
		if want, ok := funding.All()[f.currency]; !ok || want != f.id {
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

	// A new key, once there's money, goes through.
	fund(t, s, alice, 1)
	if _, created, err := postJournal(s, t.Context(), transfer("t2", alice, aaron, 101)); err != nil || !created {
		t.Fatalf("a new request after funding = %v, created %v; want it posted", err, created)
	}
	wantBalance(t, s, alice, gbp(0))
}

// TestARefusedKeyStaysRefused: a key gets one final answer. Once refused, the
// same request is refused again, even when it would go through now: its
// caller was told "refused", and a late or repeated post with that key (a
// stalled worker waking up, say) must never move the money afterwards.
func TestARefusedKeyStaysRefused(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 100)
	_, _, err := postJournal(s, t.Context(), transfer("t1", alice, bob, 101))
	wantCode(t, err, "precondition_failed.insufficient_funds")

	fund(t, s, alice, 1000) // now it would go through
	again, created, err := postJournal(s, t.Context(), transfer("t1", alice, bob, 101))
	wantCode(t, err, "precondition_failed.insufficient_funds")
	if created || again.ID != uuid.Nil() {
		t.Errorf("repeat = %+v, created %v; want the same refusal, no journal", again, created)
	}
	wantBalance(t, s, alice, gbp(1100)) // nothing moved

	// The same key for a different request is still a reused key.
	_, _, err = postJournal(s, t.Context(), transfer("t1", alice, bob, 102))
	wantCode(t, err, "precondition_failed.idempotency_key")

	var refusals int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM refusals WHERE idempotency_key = 't1'`).Scan(&refusals); err != nil || refusals != 1 {
		t.Errorf("refusals for t1 = %d, %v; want the one refusal saved once", refusals, err)
	}
}

// TestAKeyGetsOneAnswerUnderARace: many posts with one key, while the
// account is funded part way through. Each would be refused before the
// funding and posted after, but the key lock lets only the first decide,
// so they all get the same answer, and only one of a journal or a refusal
// is saved. The race is run for many rounds, each with its own key and
// account, as a single round rarely lands in the window.
func TestAKeyGetsOneAnswerUnderARace(t *testing.T) {
	s := newTestStore(t)
	gbpFund := gbpFunding(t)
	for round := range 30 {
		from, key := uuid.New(), "same-key-"+strconv.Itoa(round)
		fund(t, s, from, 100)
		answers := make([]string, 10)
		errs := runConcurrently(len(answers)+1, func(i int) error {
			if i == len(answers) {
				_, _, err := postJournal(s, t.Context(), transfer("top-up-"+key, gbpFund, from, 1000))
				return err
			}
			j, _, err := postJournal(s, t.Context(), transfer(key, from, bob, 300))
			switch {
			case err == nil:
				answers[i] = "journal " + j.ID.String()
			case hasCode(err, "precondition_failed.insufficient_funds"):
				answers[i] = "refused"
			default:
				return err
			}
			return nil
		})
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		for _, a := range answers {
			if a != answers[0] {
				t.Fatalf("round %d: one key got different answers: %v", round, answers)
			}
		}
		var journals, refusals int
		if err := s.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM journals WHERE idempotency_key = $1),
			(SELECT count(*) FROM refusals WHERE idempotency_key = $1)`, key).Scan(&journals, &refusals); err != nil {
			t.Fatal(err)
		}
		if journals+refusals != 1 {
			t.Fatalf("round %d: %d journals and %d refusals saved for one key, want exactly one answer", round, journals, refusals)
		}
	}
}

func TestPostRefusesAnotherCurrency(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 100) // alice's account now holds GBP
	eurFunding, _ := funding.Account(money.EUR())

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

// TestDepositsSpreadOverTheFundingAccountsShards: each deposit takes from
// one of the GBP funding account's 8 shards, picked at random, so busy
// deposits don't all queue for one row; its balance is the sum of them.
func TestDepositsSpreadOverTheFundingAccountsShards(t *testing.T) {
	s := newTestStore(t)
	for i := range 80 {
		if _, _, err := postJournal(s, t.Context(), transfer(fmt.Sprintf("d%d", i), gbpFunding(t), alice, 10)); err != nil {
			t.Fatal(err)
		}
	}
	var used int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(DISTINCT shard) FROM entries WHERE account_id = $1`, gbpFunding(t)).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used < 4 { // 80 deposits over 8 shards: fewer than 4 is next to impossible
		t.Errorf("80 deposits used %d of the funding account's 8 shards, want them spread", used)
	}
	wantBalance(t, s, gbpFunding(t), gbp(-800))
	wantBalance(t, s, alice, gbp(800))
}

// TestConcurrentDepositsAndTransfersDontDeadlock: deposits lock a funding
// shard in account_shards after the accounts rows they need, and transfers
// lock only accounts rows, all in one order, so a mix racing each other all
// post, and every shard still matches its entries (checkInvariants).
func TestConcurrentDepositsAndTransfersDontDeadlock(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)
	fund(t, s, bob, 1000)

	errs := runConcurrently(60, func(i int) error {
		key := fmt.Sprintf("p%d", i)
		req := map[int]NewJournal{
			0: transfer(key, gbpFunding(t), alice, 10),
			1: transfer(key, alice, bob, 10),
			2: transfer(key, bob, alice, 10),
		}[i%3]
		_, _, err := postJournal(s, t.Context(), req)
		return err
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("want every post posted: %v", err)
	}
	wantBalance(t, s, alice, gbp(1000+20*10))
	wantBalance(t, s, bob, gbp(1000))
}

// TestOnlyFundingAccountsCanBeSplit: a post locks only the one shard it
// picks, so a customer account, whose balance must be checked as a whole,
// may not be split. The ledger never splits one, and the database refuses
// it if anything else tries.
func TestOnlyFundingAccountsCanBeSplit(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 100)
	_, err := s.pool.Exec(t.Context(), `UPDATE accounts SET shards = 2 WHERE id = $1`, alice)
	if err == nil || !strings.Contains(err.Error(), "only_funding_accounts_split") {
		t.Errorf("splitting a customer account: err = %v, want only_funding_accounts_split to refuse it", err)
	}
}

// TestDepositsOnTheFundingRowAndAShardDontDeadlock: a deposit on shard 0
// locks the funding account's own row and then alice's; one on another
// shard locks alice's and then, inserting its entries, checks the funding
// row's key. With FOR UPDATE that check waits for the first, which waits for
// the second: a deadlock. Picking shard 0 and shard 1 in turn makes the two
// meet every time, not only by chance.
func TestDepositsOnTheFundingRowAndAShardDontDeadlock(t *testing.T) {
	s := newTestStore(t)
	var turn atomic.Int32
	s.pickShard = func(int16) int16 { return int16(turn.Add(1) % 2) }

	errs := runConcurrently(40, func(i int) error {
		_, _, err := postJournal(s, t.Context(), transfer(fmt.Sprintf("d%d", i), gbpFunding(t), alice, 10))
		return err
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("want every deposit posted: %v", err)
	}
	wantBalance(t, s, alice, gbp(400))
	wantBalance(t, s, gbpFunding(t), gbp(-400))
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
	_, _, err := postJournal(s, t.Context(), transfer("too-much", alice, bob, 5000)) // a refusal to protect too
	wantCode(t, err, "precondition_failed.insufficient_funds")

	for _, sql := range []string{
		`UPDATE entries SET amount = amount * 2`,
		`DELETE FROM entries`,
		`TRUNCATE entries`,
		`UPDATE journals SET currency = 'EUR'`,
		`DELETE FROM journals`,
		`TRUNCATE journals CASCADE`,
		`UPDATE refusals SET code = 'x'`,
		`DELETE FROM refusals`,
		`TRUNCATE refusals`,
	} {
		if _, err := s.pool.Exec(t.Context(), sql); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want it refused as append-only", sql, err)
		}
	}
	wantBalance(t, s, alice, gbp(1000)) // and the balance still matches its entries (checkInvariants)
}

// TestBalancesAreReadByName: kind and currency are both text, so read by
// position, swapping them in a query would make "GBP" an account's kind,
// with no error from the scan. Read by name, the order doesn't matter.
func TestBalancesAreReadByName(t *testing.T) {
	s := newTestStore(t)
	fund(t, s, alice, 1000)
	swapped := strings.Replace(accountColumns, "kind, currency", "currency, kind", 1)
	if swapped == accountColumns {
		t.Fatal("test is stale: accountColumns no longer lists kind, currency together")
	}
	got, err := db.QueryOne(t.Context(), s.pool, scanBalance, `SELECT `+swapped+` FROM accounts WHERE id = $1`, alice)
	if err != nil || got.Kind != KindCustomer || got.Amount != gbp(1000) {
		t.Errorf("with the columns swapped: %+v, %v; want a customer account holding £10", got, err)
	}
}
