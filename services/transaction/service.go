package transaction

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/monzo/terrors"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

const (
	// attemptsPerRequest is how many times a request tries the ledger before
	// answering "pending". The rest is left to the background finisher, so a
	// caller never waits long.
	attemptsPerRequest = 3
	// backoffBase and backoffMax shape the wait between attempts: about
	// 50ms, then 100ms, then 200ms, never more than 1s, each a random part of
	// that (jitter), so many transfers failing at once don't all retry in
	// step and hit the ledger together.
	backoffBase = 50 * time.Millisecond
	backoffMax  = time.Second
	// finishTimeout bounds saving the ledger's answer once it's back.
	finishTimeout = 5 * time.Second

	// The finisher looks for due transfers every finisherInterval, claiming
	// finisherBatch at a time. A claim (by a request or the finisher) holds a
	// transfer for claimLease, until its first StartAttempt; a transfer still
	// waiting in a batch when that runs out may be claimed by someone else,
	// and the finisher then skips it (its lease no longer matches).
	//
	// After StartAttempt, nextAttemptAfter keeps others off it: at least
	// 10s, more than a post to the ledger can take (kit.Post's 5s call
	// timeout), so no two posts of one transfer ever overlap.
	finisherInterval = time.Second
	finisherBatch    = 20
	claimLease       = 30 * time.Second
)

// Service is the transaction service: the Store, plus posting each pending
// transfer to the ledger until it's completed or failed. It has the Store's
// Get, and its own Create.
type Service struct {
	*Store
	ledger  Ledger
	metrics *metrics
}

// NewService returns the transaction service, saving transfers in store,
// posting them to ledger, and registering its business metrics on reg.
func NewService(store *Store, ledger Ledger, reg prometheus.Registerer) *Service {
	return &Service{Store: store, ledger: ledger, metrics: newMetrics(reg)}
}

// Create saves req as a transfer (or finds the one already saved with its
// key) and, if it's pending and due, claims it and posts it to the ledger,
// retrying what can be retried within ctx's deadline and attemptsPerRequest.
// It returns the transfer as it now stands: completed, failed, or still
// pending if the ledger couldn't be reached or didn't answer in time, or if
// another request or the finisher is already attempting it.
func (s *Service) Create(ctx context.Context, req NewTransfer) (Transfer, bool, error) {
	t, created, err := s.Store.Create(ctx, req)
	if err != nil || t.State != StatePending {
		return t, created, err
	}
	// One attempt at a time: two posts of one transfer racing could see one
	// refused and the other post the money. If it's someone else's, or
	// waiting for its next try, answer with it as it stands.
	t, claimed, err := s.Claim(ctx, t.ID, claimLease)
	if err != nil || !claimed {
		return t, created, err
	}
	t, err = s.attempt(ctx, t, byRequest, attemptsPerRequest)
	return t, created, err
}

// RunFinisher finishes the transfers requests left pending, until ctx ends:
// every finisherInterval it calls FinishDue. It's how a transfer gets
// completed or failed after the ledger was down, slow, or this service died
// mid-request. Every pod runs one; ClaimDue keeps them off each other's
// transfers.
func (s *Service) RunFinisher(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(finisherInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := s.FinishDue(ctx, logger); err != nil && ctx.Err() == nil {
			logger.Error("finish due transfers", "error", err)
		}
	}
}

// FinishDue tries each due pending transfer once more, a batch at a time,
// until none are due or ctx ends, and returns how many it tried. A transfer
// the ledger still doesn't answer stays pending, due again later
// (nextAttemptAfter). One transfer's error is logged and doesn't stop the
// others.
//
// It updates the pending metrics before each transfer, at most once every
// finisherInterval, so during an outage, when working through a backlog
// takes minutes, the dashboard still shows the count and age growing.
func (s *Service) FinishDue(ctx context.Context, logger *slog.Logger) (int, error) {
	var refreshed time.Time
	refresh := func() {
		if time.Since(refreshed) < finisherInterval {
			return
		}
		count, oldest, err := s.Pending(ctx)
		if err != nil {
			logger.Error("count pending transfers", "error", err)
			return
		}
		s.metrics.setPending(count, oldest)
		refreshed = time.Now()
	}

	tried := 0
	for ctx.Err() == nil {
		refresh()
		due, err := s.ClaimDue(ctx, finisherBatch, claimLease)
		if err != nil {
			return tried, err
		}
		for _, t := range due {
			refresh()
			s.finishOne(ctx, logger, t)
		}
		tried += len(due)
		if len(due) < finisherBatch {
			break
		}
	}
	return tried, nil
}

// tracer starts the transaction service's own spans.
var tracer = obs.NewTracer("github.com/tinypell3ts/tiny-bank/services/transaction")

// finishOne makes the finisher's attempt at t, as the root of a trace of its
// own: the finisher works outside any request, so there's no trace to join.
// The span ("finish transfer") holds t's ID and, if it has one, the trace ID
// of the request that created it, so in Jaeger a transfer's attempts can be
// found by its ID, and lead back to where it started. Its database work and
// its call to the ledger are spans under it. An error is logged with the
// attempt's trace ID.
func (s *Service) finishOne(ctx context.Context, logger *slog.Logger, t Transfer) {
	attrs := []attribute.KeyValue{attribute.String("transfer.id", t.ID.String())}
	if t.TraceID != "" {
		attrs = append(attrs, attribute.String("transfer.created_trace_id", t.TraceID))
	}
	ctx, span := tracer.Start(ctx, "finish transfer", trace.WithNewRoot(), trace.WithAttributes(attrs...))
	defer span.End()

	finished, err := s.attempt(ctx, t, byFinisher, 1)
	state := finished.State
	if state == "" { // a database error: attempt has no transfer to return, so t as claimed is the last known
		state = t.State
	}
	span.SetAttributes(attribute.String("transfer.state", string(state)))
	if err != nil {
		span.SetStatus(codes.Error, "")
		logger.ErrorContext(ctx, "finish transfer", "transfer_id", t.ID, "error", err) // the logger adds the attempt's trace_id
	}
}

// attempt posts pending transfer t, claimed under t.Lease, to the ledger, up
// to tries times, and records the outcome, counting each post as made by by.
// Before each try it records the attempt and when to try again
// (nextAttemptAfter), so if this service dies mid-call the transfer is still
// found and finished later. Every try uses the transfer's own ID as the
// ledger's idempotency key, so the money moves at most once. If the
// transfer moved on without it (finished, or claimed by another attempt), it
// stops and returns the transfer as it now stands.
func (s *Service) attempt(ctx context.Context, t Transfer, by string, tries int) (Transfer, error) {
	for try := range tries {
		started, err := s.StartAttempt(ctx, t.ID, t.Lease, nextAttemptAfter(t.Attempts+1))
		if err != nil {
			return settled(started, fmt.Errorf("start attempt: %w", err))
		}
		t = started
		journalID, postErr := s.ledger.Post(ctx, t)
		result, reason := outcomeOf(postErr)
		s.metrics.posted(by, result, reason)
		switch result {
		case posted:
			return s.record(ctx, func(ctx context.Context) (Transfer, error) { return s.Complete(ctx, t.ID, journalID) })
		case refusedForGood:
			return settled(s.record(ctx, func(ctx context.Context) (Transfer, error) { return s.Fail(ctx, t.ID, t.Lease, reason) }))
		case keyReused:
			// Our own internal error, with the ledger's kept as its cause for
			// the logs: wrapping the ledger's 412 would tell our caller their
			// key was reused, which isn't so.
			return t, terrors.NewInternalWithCause(
				fmt.Errorf("transfer %s: the ledger says its key was used for a different journal: %s", t.ID, postErr.Error()),
				"couldn't post this transfer to the ledger", nil, "ledger_key_reused")
		}

		// We can't tell whether the money moved, so try again, with the same
		// key, if there's time. Otherwise it stays pending for later.
		if try == tries-1 || !sleep(ctx, backoff(try)) {
			break
		}
	}
	return t, nil
}

// settled turns the store refusing an attempt's change, because the
// transfer moved on without it, into the transfer as it now stands: finished
// by someone else (*finishedError), or claimed by another attempt
// (errLeaseLost). That's not an error for our caller. Any other error is.
func settled(t Transfer, err error) (Transfer, error) {
	var finished *finishedError
	switch {
	case errors.As(err, &finished):
		return finished.t, nil
	case errors.Is(err, errLeaseLost):
		return t, nil
	}
	return t, err
}

// record saves the ledger's answer with save, even if our caller has hung up
// since, so a transfer the ledger posted isn't left looking pending.
func (s *Service) record(ctx context.Context, save func(context.Context) (Transfer, error)) (Transfer, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	return save(ctx)
}

// backoff is how long to wait before try number try+1: a random time up to
// backoffBase × 2^try, capped at backoffMax ("full jitter").
func backoff(try int) time.Duration {
	ceiling := min(backoffBase<<try, backoffMax)
	n, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)))
	if err != nil {
		return ceiling // crypto/rand doesn't fail on supported platforms; waiting the most is safe
	}
	return time.Duration(n.Int64())
}

// nextAttemptAfter is how long after starting attempt number attempts a
// transfer is due to be tried again, if that attempt doesn't finish it:
// longer than any one call can take, and doubling each time, from 10s up to
// 5 minutes, so a transfer the ledger keeps refusing to answer isn't retried
// in a tight loop.
func nextAttemptAfter(attempts int) time.Duration {
	return min(10*time.Second<<min(attempts-1, 10), 5*time.Minute)
}

// sleep waits for d, and reports false if ctx ends first, or would end
// before d is up (there'd be no time left to try again).
func sleep(ctx context.Context, d time.Duration) bool {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < d {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
