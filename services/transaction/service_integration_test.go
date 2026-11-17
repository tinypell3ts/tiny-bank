//go:build integration

package transaction

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/terrors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	return NewService(newTestStore(t), ledger, prometheus.NewRegistry()), ledger
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

var discard = slog.New(slog.DiscardHandler)

// leftPending creates a transfer the ledger couldn't be reached for, so it's
// pending, then makes it due, as if its next attempt time had come.
func leftPending(t *testing.T, s *Service, key string) Transfer {
	t.Helper()
	tr, _, err := s.Create(t.Context(), transferReq(key, alice, bob, 300))
	if err != nil || tr.State != StatePending {
		t.Fatalf("Create = %+v, %v; want it left pending", tr, err)
	}
	makeDue(t, s.Store, tr.ID)
	return tr
}

func TestFinishDueFinishesWhatRequestsLeftPending(t *testing.T) {
	tests := []struct {
		name        string
		finisherGot error
		wantState   State
	}{
		{"the ledger posts it", nil, StateCompleted},
		{"the ledger refuses it", errNoFunds, StateFailed},
		{"the ledger is still down", errLedgerDown, StatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answers := make([]error, attemptsPerRequest, attemptsPerRequest+1)
			for i := range answers {
				answers[i] = errUnreachable
			}
			s, ledger := newTestService(t, append(answers, tt.finisherGot)...)
			tr := leftPending(t, s, "t1")

			tried, err := s.FinishDue(t.Context(), discard)
			if err != nil || tried != 1 || ledger.calls() != attemptsPerRequest+1 {
				t.Fatalf("FinishDue = %d, %v after %d calls; want 1 tried, once", tried, err, ledger.calls())
			}
			got, _ := s.Get(t.Context(), tr.ID)
			if got.State != tt.wantState || got.Attempts != attemptsPerRequest+1 {
				t.Errorf("transfer = %s after %d attempts, want %s after %d", got.State, got.Attempts, tt.wantState, attemptsPerRequest+1)
			}
			if tt.wantState == StatePending && !got.NextAttemptAt.After(time.Now()) {
				t.Error("still pending but not scheduled again")
			}
		})
	}
}

// TestALostReplyIsReplayedWithTheSameKey: the ledger posted the journal but
// its answer never arrived. The finisher posts again with the transfer's own
// ID, which the ledger answers with the journal it already posted, so the
// money moves once and the transfer completes.
func TestALostReplyIsReplayedWithTheSameKey(t *testing.T) {
	s, ledger := newTestService(t, errUnreachable, errUnreachable, errUnreachable, nil)
	tr := leftPending(t, s, "t1")
	if _, err := s.FinishDue(t.Context(), discard); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(t.Context(), tr.ID)
	if got.State != StateCompleted || got.JournalID != ledger.journal {
		t.Fatalf("transfer = %+v, want completed with the ledger's journal", got)
	}
	for _, p := range ledger.posted {
		if p.ID != tr.ID {
			t.Fatal("the finisher posted with a different key: the money could move twice")
		}
	}
}

func TestFinishDueGoesOnPastOneTransfersError(t *testing.T) {
	s, ledger := newTestService(t, errUnreachable, errUnreachable, errUnreachable,
		errUnreachable, errUnreachable, errUnreachable, errKeyReusedBug, nil)
	first := leftPending(t, s, "t1")
	second := leftPending(t, s, "t2")
	if tried, err := s.FinishDue(t.Context(), discard); err != nil || tried != 2 {
		t.Fatalf("FinishDue = %d, %v; want both tried", tried, err)
	}
	a, _ := s.Get(t.Context(), first.ID)
	b, _ := s.Get(t.Context(), second.ID)
	if a.State != StatePending || b.State != StateCompleted || ledger.calls() != 8 {
		t.Errorf("states %s, %s after %d calls; want the bug left pending and the other completed", a.State, b.State, ledger.calls())
	}
}

func TestFinishDueWorksThroughMoreThanOneBatch(t *testing.T) {
	s, ledger := newTestService(t, nil)
	for i := range finisherBatch + 5 {
		tr, _, err := s.Store.Create(t.Context(), transferReq("t"+strconv.Itoa(i), alice, bob, 300))
		if err != nil {
			t.Fatal(err)
		}
		makeDue(t, s.Store, tr.ID)
	}
	if tried, err := s.FinishDue(t.Context(), discard); err != nil || tried != finisherBatch+5 || ledger.calls() != finisherBatch+5 {
		t.Errorf("FinishDue = %d, %v, %d calls; want all %d", tried, err, ledger.calls(), finisherBatch+5)
	}
}

func TestRunFinisherFinishesThenStopsAtShutdown(t *testing.T) {
	s, _ := newTestService(t, errUnreachable, errUnreachable, errUnreachable, nil)
	tr := leftPending(t, s, "t1")
	ctx, shutdown := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.RunFinisher(ctx, discard); close(done) }()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if got, _ := s.Get(t.Context(), tr.ID); got.State == StateCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the finisher never completed the transfer")
		}
	}
	shutdown()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunFinisher didn't stop at shutdown")
	}
}

// TestTheMetricsSayWhoTriedAndWhatsStuck: a request's posts and the
// finisher's are told apart, and the finisher reports what's still pending.
func TestTheMetricsSayWhoTriedAndWhatsStuck(t *testing.T) {
	s, _ := newTestService(t, errUnreachable, errUnreachable, errUnreachable, errLedgerDown)
	leftPending(t, s, "t1")
	if _, err := s.FinishDue(t.Context(), discard); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(s.metrics.posts.WithLabelValues(byRequest, "unsure", "")); got != attemptsPerRequest {
		t.Errorf("request posts = %v, want %d", got, attemptsPerRequest)
	}
	if got := testutil.ToFloat64(s.metrics.posts.WithLabelValues(byFinisher, "unsure", "")); got != 1 {
		t.Errorf("finisher posts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.metrics.pending); got != 1 {
		t.Errorf("pending = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.metrics.oldestPending); got <= 0 {
		t.Errorf("oldest pending = %vs, want its age", got)
	}
}
