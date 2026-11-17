//go:build integration

package transaction

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(dbtest.Migrated(t, Migrations()))
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
	tr := mustCreate(t, s, transferReq("t1", alice, bob, 300))
	got, err := s.StartAttempt(t.Context(), tr.ID, time.Minute)
	if err != nil || got.Attempts != 1 {
		t.Fatalf("StartAttempt = %+v, %v; want 1 attempt", got, err)
	}
	if wait := got.NextAttemptAt.Sub(got.UpdatedAt); wait < 59*time.Second || wait > 61*time.Second {
		t.Errorf("next attempt in %v, want about a minute", wait)
	}
	if got, _ := s.StartAttempt(t.Context(), tr.ID, time.Minute); got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
}

func TestCompleteIsFinal(t *testing.T) {
	s := newTestStore(t)
	tr := mustCreate(t, s, transferReq("t1", alice, bob, 300))
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
	_, errFail := s.Fail(t.Context(), tr.ID, "precondition_failed.insufficient_funds")
	_, errRetry := s.StartAttempt(t.Context(), tr.ID, time.Minute)
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

func TestFailIsFinal(t *testing.T) {
	s := newTestStore(t)
	tr := mustCreate(t, s, transferReq("t1", alice, bob, 300))
	if got, err := s.Fail(t.Context(), tr.ID, "precondition_failed.insufficient_funds"); err != nil || got.State != StateFailed {
		t.Fatalf("Fail = %+v, %v", got, err)
	}
	if _, err := s.Fail(t.Context(), tr.ID, "precondition_failed.insufficient_funds"); err != nil {
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
