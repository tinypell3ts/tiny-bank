//go:build integration

package transaction

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/terrors"
)

// fakeLedger answers posts from a script: each call takes the next answer,
// and the last answer repeats. It records every transfer posted to it, so
// tests can check the key and how many times it was tried.
type fakeLedger struct {
	mu      sync.Mutex
	answers []error // nil means "posted"
	journal uuid.UUID
	posted  []Transfer
}

func (f *fakeLedger) Post(_ context.Context, t Transfer) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posted = append(f.posted, t)
	err := f.answers[min(len(f.posted), len(f.answers))-1]
	if err != nil {
		return uuid.Nil(), err
	}
	return f.journal, nil
}

func (f *fakeLedger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posted)
}

func newTestService(t *testing.T, answers ...error) (*Service, *fakeLedger) {
	t.Helper()
	ledger := &fakeLedger{answers: answers, journal: uuid.New()}
	return NewService(newTestStore(t), ledger), ledger
}

var (
	errUnreachable  = errors.New("dial tcp: connection refused")
	errLedgerDown   = terrors.InternalService("account_service", "couldn't check an account", nil)
	errNoFunds      = terrors.PreconditionFailed("insufficient_funds", "account doesn't hold enough money", nil)
	errKeyReusedBug = terrors.PreconditionFailed("idempotency_key", "this idempotency key was already used", nil)
)

func TestCreateCompletesATransfer(t *testing.T) {
	s, ledger := newTestService(t, nil)
	tr, created, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || !created || tr.State != StateCompleted || tr.JournalID != ledger.journal || tr.Attempts != 1 {
		t.Fatalf("Create = %+v, %v, %v; want completed with the ledger's journal after 1 attempt", tr, created, err)
	}
	if ledger.posted[0].ID != tr.ID {
		t.Error("posted with something other than the transfer's own ID as the key")
	}
}

func TestCreateFailsARefusedTransfer(t *testing.T) {
	s, ledger := newTestService(t, errNoFunds)
	tr, _, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || tr.State != StateFailed || tr.FailureReason != "precondition_failed.insufficient_funds" || ledger.calls() != 1 {
		t.Fatalf("Create = %+v, %v; want failed for insufficient funds, tried once (a refusal isn't retried)", tr, err)
	}
}

// TestCreateRetriesWhatMightHaveWorked: a timeout or 5xx might have moved the
// money, so it's tried again with the same key, never marked failed.
func TestCreateRetriesWhatMightHaveWorked(t *testing.T) {
	s, ledger := newTestService(t, errUnreachable, errLedgerDown, nil)
	tr, _, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || tr.State != StateCompleted || tr.Attempts != 3 || ledger.calls() != 3 {
		t.Fatalf("Create = %+v, %v, %d calls; want completed on the 3rd attempt", tr, err, ledger.calls())
	}
	for _, p := range ledger.posted {
		if p.ID != tr.ID {
			t.Fatal("a retry used a different key: the money could move twice")
		}
	}
}

func TestCreateLeavesItPendingWhenTheLedgerCantBeReached(t *testing.T) {
	s, ledger := newTestService(t, errUnreachable)
	before := time.Now()
	tr, _, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || tr.State != StatePending || tr.Attempts != attemptsPerRequest || ledger.calls() != attemptsPerRequest {
		t.Fatalf("Create = %+v, %v; want pending after %d attempts", tr, err, attemptsPerRequest)
	}
	if !tr.NextAttemptAt.After(before) {
		t.Error("no next attempt scheduled, so nothing would finish it")
	}
}

func TestCreateStopsRetryingAtTheCallersDeadline(t *testing.T) {
	s, ledger := newTestService(t, errUnreachable)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	tr, _, err := s.Create(ctx, transferReq("t1", alice, bob, 300))
	if err == nil && tr.State != StatePending {
		t.Fatalf("Create = %+v; want it left pending", tr)
	}
	if ledger.calls() > 1 {
		t.Errorf("%d calls after the deadline passed, want no retries", ledger.calls())
	}
}

// TestAReusedLedgerKeyIsABugNotAFailure: the money may have moved under the
// other journal, so the transfer must not be marked failed, and the caller
// must not be told their key was reused.
func TestAReusedLedgerKeyIsABugNotAFailure(t *testing.T) {
	s, ledger := newTestService(t, errKeyReusedBug)
	tr, _, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "internal_service.ledger_key_reused" {
		t.Fatalf("err = %v, want our own internal_service.ledger_key_reused", err)
	}
	if got, _ := s.Get(t.Context(), tr.ID); got.State != StatePending || ledger.calls() != 1 {
		t.Errorf("transfer = %s after %d calls, want pending, tried once", got.State, ledger.calls())
	}
}

func TestARetryOfAFinishedTransferDoesntCallTheLedger(t *testing.T) {
	s, ledger := newTestService(t, nil)
	first, _, _ := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	again, created, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || created || again.ID != first.ID || again.State != StateCompleted || ledger.calls() != 1 {
		t.Fatalf("retry = %+v, %v, %v, %d calls; want the first transfer, no new call", again, created, err, ledger.calls())
	}
}

func TestARetryOfAPendingTransferTriesAgain(t *testing.T) {
	s, _ := newTestService(t, errUnreachable, errUnreachable, errUnreachable, nil)
	first, _, _ := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if first.State != StatePending {
		t.Fatalf("first = %s, want pending", first.State)
	}
	again, _, err := s.Create(t.Context(), transferReq("t1", alice, bob, 300))
	if err != nil || again.ID != first.ID || again.State != StateCompleted || again.Attempts != 4 {
		t.Fatalf("retry = %+v, %v; want the same transfer completed on attempt 4", again, err)
	}
}

func TestBackoffStaysUnderItsCeiling(t *testing.T) {
	for try := range 10 {
		ceiling := min(backoffBase<<try, backoffMax)
		for range 50 {
			if d := backoff(try); d < 0 || d > ceiling {
				t.Fatalf("backoff(%d) = %v, want 0 to %v", try, d, ceiling)
			}
		}
	}
	if nextAttemptAfter(1) != 10*time.Second || nextAttemptAfter(100) != 5*time.Minute {
		t.Errorf("nextAttemptAfter = %v, %v; want 10s, then capped at 5m", nextAttemptAfter(1), nextAttemptAfter(100))
	}
}
