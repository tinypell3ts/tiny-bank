package transaction

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/monzo/terrors"
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
	// finisherBatch at a time, each for claimLease: longer than one attempt
	// takes, so a claimed transfer is never tried twice at once.
	finisherInterval = time.Second
	finisherBatch    = 20
	claimLease       = 30 * time.Second
)

// Service is the transaction service: the Store, plus posting each pending
// transfer to the ledger until it's completed or failed. It has the Store's
// Get, and its own Create.
type Service struct {
	*Store
	ledger Ledger
}

// NewService returns the transaction service, saving transfers in store and
// posting them to ledger.
func NewService(store *Store, ledger Ledger) *Service {
	return &Service{Store: store, ledger: ledger}
}

// Create saves req as a transfer (or finds the one already saved with its
// key) and, if it's pending, posts it to the ledger, retrying what can be
// retried within ctx's deadline and attemptsPerRequest. It returns the
// transfer as it now stands: completed, failed, or still pending if the
// ledger couldn't be reached or didn't answer in time.
func (s *Service) Create(ctx context.Context, req NewTransfer) (Transfer, bool, error) {
	t, created, err := s.Store.Create(ctx, req)
	if err != nil || t.State != StatePending {
		return t, created, err
	}
	t, err = s.attempt(ctx, t, attemptsPerRequest)
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
func (s *Service) FinishDue(ctx context.Context, logger *slog.Logger) (int, error) {
	tried := 0
	for ctx.Err() == nil {
		due, err := s.ClaimDue(ctx, finisherBatch, claimLease)
		if err != nil {
			return tried, err
		}
		for _, t := range due {
			if _, err := s.attempt(ctx, t, 1); err != nil {
				logger.Error("finish transfer", "transfer_id", t.ID, "error", err)
			}
		}
		tried += len(due)
		if len(due) < finisherBatch {
			break
		}
	}
	return tried, nil
}

// attempt posts pending transfer t to the ledger, up to tries times, and
// records the outcome. Before each try it records the attempt and
// when to try again (nextAttemptAfter), so if this service dies mid-call the
// transfer is still found and finished later. Every try uses the transfer's
// own ID as the ledger's idempotency key, so the money moves at most once.
func (s *Service) attempt(ctx context.Context, t Transfer, tries int) (Transfer, error) {
	for try := range tries {
		var err error
		if t, err = s.StartAttempt(ctx, t.ID, nextAttemptAfter(t.Attempts+1)); err != nil {
			return t, fmt.Errorf("start attempt: %w", err)
		}
		journalID, postErr := s.ledger.Post(ctx, t)
		switch result, reason := outcomeOf(postErr); result {
		case posted:
			return s.record(ctx, func(ctx context.Context) (Transfer, error) { return s.Complete(ctx, t.ID, journalID) })
		case refusedForGood:
			return s.record(ctx, func(ctx context.Context) (Transfer, error) { return s.Fail(ctx, t.ID, reason) })
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
