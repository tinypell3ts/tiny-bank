//go:build integration

package transaction

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/monzo/terrors"
	"google.golang.org/protobuf/proto"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	transactionv1 "github.com/tinypell3ts/tiny-bank/proto/transaction/v1"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(dbtest.Migrated(t, Migrations()), AnnounceTransferChanged)
}

func hasCode(err error, code string) bool {
	var terr *terrors.Error
	return errors.As(err, &terr) && terr.Code == code
}

func mustCreate(t *testing.T, s *Store, req NewTransfer) Transfer {
	t.Helper()
	tr, created, err := s.Create(t.Context(), req)
	if err != nil || !created {
		t.Fatalf("Create = %v, created %v; want a new transfer", err, created)
	}
	return tr
}

// mustClaim claims transfer id, which must be due, and returns it with its
// lease.
func mustClaim(t *testing.T, s *Store, id uuid.UUID) Transfer {
	t.Helper()
	tr, ok, err := s.Claim(t.Context(), id, time.Minute)
	if err != nil || !ok || tr.Lease == uuid.Nil() {
		t.Fatalf("Claim = %+v, %v, %v; want it claimed with a lease", tr, ok, err)
	}
	return tr
}

func TestCreateSavesAPendingTransfer(t *testing.T) {
	s := newTestStore(t)
	before := time.Now().Add(-time.Minute)
	tr := mustCreate(t, s, transferReq("t1", alice, bob, 300))
	if tr.ID == uuid.Nil() || tr.State != StatePending || tr.Attempts != 0 || tr.CreatedAt.Before(before) ||
		tr.From != alice || tr.To != bob || tr.Amount != gbp(300) || tr.Kind != KindTransfer {
		t.Errorf("created = %+v, want a pending transfer of £3 from alice to bob", tr)
	}
	got, err := s.Get(t.Context(), tr.ID)
	if err != nil || got.ID != tr.ID || got.IdempotencyKey != "t1" {
		t.Errorf("Get = %+v, %v; want it back", got, err)
	}

	d := mustCreate(t, s, depositReq("d1", alice, 1000))
	if d.Kind != KindDeposit || d.From != uuid.Nil() {
		t.Errorf("deposit = %+v, want no from account", d)
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	first := mustCreate(t, s, transferReq("t1", alice, bob, 300))

	again, created, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry = %v, created %v; want the first transfer", err, created)
	}
	for name, other := range map[string]NewTransfer{
		"another amount":  transferReq("t1", alice, bob, 301),
		"another account": transferReq("t1", alice, uuid.New(), 300),
		"a deposit":       depositReq("t1", bob, 300),
	} {
		if _, _, err := s.Create(t.Context(), other); !hasCode(err, "precondition_failed.idempotency_key") {
			t.Errorf("%s with the same key: err = %v, want precondition_failed.idempotency_key", name, err)
		}
	}
}

func TestConcurrentCreatesWithOneKeySaveOne(t *testing.T) {
	s := newTestStore(t)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[uuid.UUID]bool{}
	)
	for range 20 {
		wg.Go(func() {
			tr, c, err := s.Create(t.Context(), transferReq("same", alice, bob, 300))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[tr.ID] = true
			if c {
				created++
			}
		})
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Errorf("%d created, %d different transfers; want exactly one", created, len(ids))
	}
}

func TestStartAttemptCountsAndSchedulesTheNextOne(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	got, err := s.StartAttempt(t.Context(), tr.ID, tr.Lease, time.Minute)
	if err != nil || got.Attempts != 1 {
		t.Fatalf("StartAttempt = %+v, %v; want 1 attempt", got, err)
	}
	if wait := got.NextAttemptAt.Sub(got.UpdatedAt); wait < 59*time.Second || wait > 61*time.Second {
		t.Errorf("next attempt in %v, want about a minute", wait)
	}
	if got, _ := s.StartAttempt(t.Context(), tr.ID, tr.Lease, time.Minute); got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
}

func TestCompleteIsFinal(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	journal := uuid.New()

	done, err := s.Complete(t.Context(), tr.ID, journal)
	if err != nil || done.State != StateCompleted || done.JournalID != journal {
		t.Fatalf("Complete = %+v, %v; want completed with the journal", done, err)
	}
	// The same answer arriving twice is fine.
	if again, err := s.Complete(t.Context(), tr.ID, journal); err != nil || again.JournalID != journal {
		t.Errorf("completing again with the same journal = %v, want it accepted", err)
	}
	// Anything else is a bug, and must not change the transfer.
	_, errOther := s.Complete(t.Context(), tr.ID, uuid.New())
	_, errFail := s.Fail(t.Context(), tr.ID, tr.Lease, "precondition_failed.insufficient_funds")
	_, errRetry := s.StartAttempt(t.Context(), tr.ID, tr.Lease, time.Minute)
	for name, err := range map[string]error{"another journal": errOther, "failing it": errFail, "another attempt": errRetry} {
		var finished *finishedError
		if !errors.As(err, &finished) {
			t.Errorf("%s on a completed transfer: err = %v, want a finished-transfer error", name, err)
		}
	}
	if got, _ := s.Get(t.Context(), tr.ID); got.State != StateCompleted || got.JournalID != journal || got.Attempts != 0 {
		t.Errorf("transfer changed after it was completed: %+v", got)
	}
}

// announced is what's in the outbox for transfer id: the states of its
// TransferChanged events, oldest first, and their event IDs.
func announced(t *testing.T, s *Store, id uuid.UUID) (states []transactionv1.TransferState, eventIDs []string) {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT topic, value FROM outbox WHERE key = $1 ORDER BY id`, []byte(id.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var topic string
		var value []byte
		var e transactionv1.TransferChanged
		if err := rows.Scan(&topic, &value); err != nil || proto.Unmarshal(value, &e) != nil || topic != TransfersTopic {
			t.Fatalf("outbox row: %v, topic %q, want a TransferChanged on %s", err, topic, TransfersTopic)
		}
		if key := e.GetTransfer().GetIdempotencyKey(); key != "" {
			t.Errorf("an event carries the caller's idempotency key %q, want it left out", key)
		}
		states = append(states, e.GetTransfer().GetState())
		eventIDs = append(eventIDs, e.GetEventId())
	}
	return states, eventIDs
}

// TestOnlyTheCallThatChangesATransferAnnouncesIt: creating, completing and
// failing each add one event in the same transaction; a retry, the same
// answer again, a refused change or an attempt starting add none.
func TestOnlyTheCallThatChangesATransferAnnouncesIt(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	if _, _, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAttempt(t.Context(), tr.ID, tr.Lease, time.Minute); err != nil {
		t.Fatal(err)
	}
	journal := uuid.New()
	for range 2 {
		if _, err := s.Complete(t.Context(), tr.ID, journal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Fail(t.Context(), tr.ID, tr.Lease, "precondition_failed.insufficient_funds"); err == nil {
		t.Fatal("Fail on a completed transfer: err = nil, want a finished-transfer error")
	}

	states, eventIDs := announced(t, s, tr.ID)
	if len(states) != 2 || states[0] != transactionv1.TransferState_TRANSFER_STATE_PENDING ||
		states[1] != transactionv1.TransferState_TRANSFER_STATE_COMPLETED || eventIDs[0] == eventIDs[1] {
		t.Errorf("announced %v (event IDs %v), want pending then completed, with their own IDs", states, eventIDs)
	}

	refused := mustClaim(t, s, mustCreate(t, s, transferReq("t2", alice, bob, 300)).ID)
	if _, err := s.Fail(t.Context(), refused.ID, refused.Lease, "precondition_failed.insufficient_funds"); err != nil {
		t.Fatal(err)
	}
	if states, _ := announced(t, s, refused.ID); len(states) != 2 || states[1] != transactionv1.TransferState_TRANSFER_STATE_FAILED {
		t.Errorf("a refused transfer announced %v, want pending then failed", states)
	}
}

// TestAChangeWhoseAnnouncementFailsIsNotSaved: the change and its event are
// one transaction, so if saving the event fails, the change doesn't happen
// either; tried again, it happens once, with one event.
func TestAChangeWhoseAnnouncementFailsIsNotSaved(t *testing.T) {
	pool := dbtest.Migrated(t, Migrations())
	failing := NewStore(pool, func(ctx context.Context, tx pgx.Tx, tr Transfer) error {
		if err := AnnounceTransferChanged(ctx, tx, tr); err != nil { // the event is written...
			return err
		}
		return errors.New("outbox full") // ...then the announcement fails
	})
	working := NewStore(pool, AnnounceTransferChanged)
	countOutbox := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, _, err := failing.Create(t.Context(), transferReq("t1", alice, bob, 300)); err == nil {
		t.Fatal("Create with a failing announcement: err = nil, want an error")
	}
	if n := countOutbox(); n != 0 {
		t.Errorf("%d events after a failed Create, want 0", n)
	}
	tr := mustClaim(t, working, mustCreate(t, working, transferReq("t1", alice, bob, 300)).ID)

	journal := uuid.New()
	if _, err := failing.Complete(t.Context(), tr.ID, journal); err == nil {
		t.Fatal("Complete with a failing announcement: err = nil, want an error")
	}
	if _, err := failing.Fail(t.Context(), tr.ID, tr.Lease, "precondition_failed.insufficient_funds"); err == nil {
		t.Fatal("Fail with a failing announcement: err = nil, want an error")
	}
	if got, _ := working.Get(t.Context(), tr.ID); got.State != StatePending {
		t.Errorf("after failed announcements the transfer is %s, want still pending", got.State)
	}
	if n := countOutbox(); n != 1 {
		t.Errorf("%d events after failed Complete and Fail, want just the creation's", n)
	}

	if _, err := working.Complete(t.Context(), tr.ID, journal); err != nil {
		t.Fatal(err)
	}
	if states, _ := announced(t, working, tr.ID); len(states) != 2 || states[1] != transactionv1.TransferState_TRANSFER_STATE_COMPLETED {
		t.Errorf("announced %v, want pending then completed, once each", states)
	}
}

func TestFailIsFinal(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	if got, err := s.Fail(t.Context(), tr.ID, tr.Lease, "precondition_failed.insufficient_funds"); err != nil || got.State != StateFailed {
		t.Fatalf("Fail = %+v, %v", got, err)
	}
	if _, err := s.Fail(t.Context(), tr.ID, tr.Lease, "precondition_failed.insufficient_funds"); err != nil {
		t.Errorf("failing again for the same reason = %v, want it accepted", err)
	}
	var finished *finishedError
	if _, err := s.Complete(t.Context(), tr.ID, uuid.New()); !errors.As(err, &finished) {
		t.Errorf("completing a failed transfer: err = %v, want a finished-transfer error", err)
	}
}

func TestNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get(t.Context(), uuid.New()); !hasCode(err, "not_found.transfer") {
		t.Errorf("Get: err = %v, want not_found.transfer", err)
	}
	if _, err := s.Complete(t.Context(), uuid.New(), uuid.New()); !hasCode(err, "not_found.transfer") {
		t.Errorf("Complete: err = %v, want not_found.transfer", err)
	}
}

// TestTheTableKeepsTheRules: even SQL that skips the Go code can't store an
// impossible transfer or change a finished one.
func TestTheTableKeepsTheRules(t *testing.T) {
	s := newTestStore(t)
	tr := mustCreate(t, s, transferReq("t1", alice, bob, 300))
	if _, err := s.Complete(t.Context(), tr.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE transfers SET amount = 1 WHERE idempotency_key = 't1'`,                                                                                                                                                  // a finished transfer
		`UPDATE transfers SET state = 'pending', journal_id = NULL WHERE idempotency_key = 't1'`,                                                                                                                        // back to pending
		`INSERT INTO transfers (idempotency_key, kind, to_account, currency, amount, state) VALUES ('x1', 'deposit', gen_random_uuid(), 'GBP', 1, 'completed')`,                                                         // no journal
		`INSERT INTO transfers (idempotency_key, kind, from_account, to_account, currency, amount) VALUES ('x2', 'deposit', gen_random_uuid(), gen_random_uuid(), 'GBP', 1)`,                                            // a deposit with a from
		`INSERT INTO transfers (idempotency_key, kind, from_account, to_account, currency, amount) VALUES ('x3', 'transfer', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', 'GBP', 1)`, // to itself
		`INSERT INTO transfers (idempotency_key, kind, to_account, currency, amount) VALUES ('x4', 'deposit', gen_random_uuid(), 'GBP', 0)`,                                                                             // zero
		`INSERT INTO transfers (idempotency_key, kind, to_account, currency, amount) VALUES ('bad key', 'deposit', gen_random_uuid(), 'GBP', 1)`,                                                                        // a bad key
	} {
		if _, err := s.pool.Exec(t.Context(), sql); err == nil {
			t.Errorf("accepted %s", strings.Fields(sql)[0]+" … "+sql[len(sql)-40:])
		}
	}
}

// makeDue sets transfer id due now, as if its next attempt time had passed.
func makeDue(t *testing.T, s *Store, id uuid.UUID) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE transfers SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestClaimDueClaimsOnlyDuePendingTransfers(t *testing.T) {
	s := newTestStore(t)
	due := mustCreate(t, s, transferReq("due", alice, bob, 1))
	notYet := mustClaim(t, s, mustCreate(t, s, transferReq("not-yet", alice, bob, 2)).ID)
	if _, err := s.StartAttempt(t.Context(), notYet.ID, notYet.Lease, time.Hour); err != nil {
		t.Fatal(err)
	}
	done := mustCreate(t, s, transferReq("done", alice, bob, 3))
	makeDue(t, s, done.ID)
	if _, err := s.Complete(t.Context(), done.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	claimed, err := s.ClaimDue(t.Context(), 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != due.ID || claimed[0].Lease == uuid.Nil() {
		t.Fatalf("ClaimDue = %v, %v; want only the due pending transfer, with a lease", claimed, err)
	}
	if !claimed[0].NextAttemptAt.After(before.Add(50 * time.Second)) {
		t.Errorf("next attempt %v, want it pushed a lease ahead", claimed[0].NextAttemptAt)
	}
	if again, err := s.ClaimDue(t.Context(), 10, time.Minute); err != nil || len(again) != 0 {
		t.Errorf("second ClaimDue = %v, %v; want nothing while the claim lasts", again, err)
	}
}

func TestClaimDueTakesTheOldestFirstUpToTheLimit(t *testing.T) {
	s := newTestStore(t)
	oldest := mustCreate(t, s, transferReq("t1", alice, bob, 1))
	mustCreate(t, s, transferReq("t2", alice, bob, 2))
	claimed, err := s.ClaimDue(t.Context(), 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != oldest.ID {
		t.Fatalf("ClaimDue = %v, %v; want just the oldest", claimed, err)
	}
}

// TestFinishersNeverClaimTheSameTransfer: two pods' finishers claiming at the
// same moment get different transfers (SKIP LOCKED), so none is tried twice
// at once.
func TestFinishersNeverClaimTheSameTransfer(t *testing.T) {
	s := newTestStore(t)
	const n = 40
	for i := range n {
		mustCreate(t, s, transferReq("t"+strconv.Itoa(i), alice, bob, int64(i+1)))
	}
	var (
		mu    sync.Mutex
		seen  = map[uuid.UUID]int{}
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for range 4 {
		wg.Go(func() {
			<-start
			for {
				claimed, err := s.ClaimDue(t.Context(), 3, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, c := range claimed {
					seen[c.ID]++
				}
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	if len(seen) != n {
		t.Errorf("claimed %d transfers, want all %d", len(seen), n)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("transfer %s claimed %d times, want once", id, times)
		}
	}
}

func TestClaimTakesATransferOnlyWhenItsDue(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	// Claimed: not due again until the claim runs out, so nobody else gets it.
	again, ok, err := s.Claim(t.Context(), tr.ID, time.Minute)
	if err != nil || ok || again.ID != tr.ID || again.Lease != tr.Lease {
		t.Fatalf("second Claim = %+v, %v, %v; want it refused, with the transfer as it stands", again, ok, err)
	}
	// Once due again, it's claimed under a new lease.
	makeDue(t, s, tr.ID)
	if next := mustClaim(t, s, tr.ID); next.Lease == tr.Lease {
		t.Error("a new claim kept the old lease")
	}
}

func TestAFinishedTransferCantBeClaimed(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	makeDue(t, s, tr.ID) // due, so only its being finished stops the claim
	if _, err := s.Complete(t.Context(), tr.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Claim(t.Context(), tr.ID, time.Minute); err != nil || ok || got.State != StateCompleted {
		t.Errorf("Claim = %+v, %v, %v; want it refused, with the completed transfer", got, ok, err)
	}
}

// TestOnlyTheLeaseHolderCanAttemptOrRefuse: an attempt whose claim ran out,
// and was taken by another, may neither post again nor record a refusal: the
// other attempt may have moved the money.
func TestOnlyTheLeaseHolderCanAttemptOrRefuse(t *testing.T) {
	s := newTestStore(t)
	first := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	makeDue(t, s, first.ID) // first's claim ran out
	second := mustClaim(t, s, first.ID)

	if got, err := s.StartAttempt(t.Context(), first.ID, first.Lease, time.Minute); !errors.Is(err, errLeaseLost) || got.Lease != second.Lease {
		t.Errorf("StartAttempt with the old lease = %+v, %v; want errLeaseLost with the transfer as it stands", got, err)
	}
	if _, err := s.Fail(t.Context(), first.ID, first.Lease, "precondition_failed.insufficient_funds"); !errors.Is(err, errLeaseLost) {
		t.Errorf("Fail with the old lease = %v, want errLeaseLost", err)
	}
	if got, _ := s.Get(t.Context(), first.ID); got.State != StatePending || got.Attempts != 0 {
		t.Errorf("transfer = %s after %d attempts; want it untouched by the old lease", got.State, got.Attempts)
	}
	if got, err := s.Fail(t.Context(), second.ID, second.Lease, "precondition_failed.insufficient_funds"); err != nil || got.State != StateFailed {
		t.Errorf("Fail with the current lease = %+v, %v; want it failed", got, err)
	}
}

// TestATransferKeepsTheIDsOfTheRequestThatCreatedIt: they lead from the
// transfer to its trace and logs, so they're saved with it, and a retry from
// another request doesn't replace them.
func TestATransferKeepsTheIDsOfTheRequestThatCreatedIt(t *testing.T) {
	s := newTestStore(t)
	req := transferReq("t1", alice, bob, 300)
	req.TraceID, req.RequestID = "4bf92f3577b34da6a3ce929d0e0e4736", "req-1"
	first := mustCreate(t, s, req)
	if got, _ := s.Get(t.Context(), first.ID); got.TraceID != req.TraceID || got.RequestID != "req-1" {
		t.Fatalf("saved trace %q, request %q; want the creating request's", got.TraceID, got.RequestID)
	}

	retry := transferReq("t1", alice, bob, 300)
	retry.TraceID, retry.RequestID = "0af7651916cd43dd8448eb211c80319c", "req-2"
	again, created, err := s.Create(t.Context(), retry)
	if err != nil || created || again.TraceID != req.TraceID || again.RequestID != "req-1" {
		t.Errorf("retry = trace %q, request %q, %v, created %v; want the first request's IDs kept", again.TraceID, again.RequestID, err, created)
	}

	if none := mustCreate(t, s, transferReq("t2", alice, bob, 300)); none.TraceID != "" || none.RequestID != "" {
		t.Errorf("a transfer from a request with no IDs = %q, %q; want both empty", none.TraceID, none.RequestID)
	}
}

// TestRowsAreReadByName: created_at and updated_at are both timestamps, so
// read by position, swapping them in a query would put each in the other's
// field without any error. Read by name, the order doesn't matter, and a
// query missing a column is an error, not a field quietly left empty.
func TestRowsAreReadByName(t *testing.T) {
	s := newTestStore(t)
	tr := mustClaim(t, s, mustCreate(t, s, transferReq("t1", alice, bob, 300)).ID)
	if _, err := s.StartAttempt(t.Context(), tr.ID, tr.Lease, time.Minute); err != nil { // moves updated_at on
		t.Fatal(err)
	}
	want, err := s.Get(t.Context(), tr.ID)
	if err != nil || !want.UpdatedAt.After(want.CreatedAt) {
		t.Fatalf("Get = %+v, %v; want updated_at after created_at, so a swap would show", want, err)
	}

	swapped := strings.Replace(columns, "created_at, updated_at", "updated_at, created_at", 1)
	if swapped == columns {
		t.Fatal("test is stale: columns no longer lists created_at, updated_at together")
	}
	got, err := s.queryTransfer(t.Context(), `SELECT `+swapped+` FROM transfers WHERE id = $1`, tr.ID)
	if err != nil || !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("with the columns swapped: created %v, updated %v, %v; want %v, %v", got.CreatedAt, got.UpdatedAt, err, want.CreatedAt, want.UpdatedAt)
	}

	missing := strings.Replace(columns, ", trace_id", "", 1)
	if _, err := s.queryTransfer(t.Context(), `SELECT `+missing+` FROM transfers WHERE id = $1`, tr.ID); err == nil {
		t.Error("a query missing trace_id was read without an error, want one")
	}
}
