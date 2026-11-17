package transaction

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// Store saves transfers and moves them through their states, in the
// service's Postgres database. Every query passes values as parameters, never
// by building SQL from strings.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store using pool. The schema must already be migrated.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// columns is selected by every query, in the order scanTransfer reads them.
const columns = `id, idempotency_key, kind, from_account, to_account, currency, amount, state,
	journal_id, failure_reason, attempts, next_attempt_at, created_at, updated_at`

// Create saves req as a new pending transfer. created is false when a
// transfer with req's idempotency key already exists and asks for the same
// movement of money: that transfer is returned, whatever state it's in, and
// nothing new is saved. The same key for a different movement is a terrors
// precondition_failed.idempotency_key.
//
// The transfer is saved before anything is sent to the ledger, so a crash
// right after leaves a pending transfer to finish, not money nobody knows
// about.
func (s *Store) Create(ctx context.Context, req NewTransfer) (Transfer, bool, error) {
	if err := req.Validate(); err != nil {
		return Transfer{}, false, fmt.Errorf("create transfer: %w", err)
	}
	var from *uuid.UUID // NULL for a deposit
	if req.Kind == KindTransfer {
		from = &req.From
	}
	t, err := scanTransfer(s.pool.QueryRow(ctx,
		`INSERT INTO transfers (idempotency_key, kind, from_account, to_account, currency, amount)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING `+columns,
		req.IdempotencyKey, req.Kind, from, req.To, req.Amount.Currency().Code(), req.Amount.Amount()))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, false, fmt.Errorf("create transfer: %w", err)
	}

	// The key is taken: a retry, or a key reused for something else.
	existing, err := scanTransfer(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM transfers WHERE idempotency_key = $1`, req.IdempotencyKey))
	if err != nil {
		return Transfer{}, false, fmt.Errorf("create transfer: read transfer for key: %w", err)
	}
	if !req.same(existing) {
		return Transfer{}, false, fmt.Errorf("create transfer: %w", errKeyReused())
	}
	return existing, false, nil
}

// Get returns the transfer with id, or a terrors not_found.transfer.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Transfer, error) {
	t, err := scanTransfer(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM transfers WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, errNotFound()
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("get transfer: %w", err)
	}
	return t, nil
}

// StartAttempt records that pending transfer id is about to be sent to the
// ledger: it counts the attempt, and sets when it's next due if this one
// doesn't finish it (after retryAfter). Recording it first means that if the
// service dies mid-attempt, the transfer is still picked up and tried again.
func (s *Store) StartAttempt(ctx context.Context, id uuid.UUID, retryAfter time.Duration) (Transfer, error) {
	return s.change(ctx, id, "start attempt on",
		`UPDATE transfers SET attempts = attempts + 1, next_attempt_at = now() + $2::interval, updated_at = now()
		 WHERE id = $1 AND state = 'pending' RETURNING `+columns,
		id, retryAfter)
}

// Complete marks pending transfer id completed, posted by the ledger as
// journal journalID. Completing it again with the same journal is fine (an
// answer that arrived twice); anything else on a finished transfer is an
// error, since a finished transfer never changes.
func (s *Store) Complete(ctx context.Context, id, journalID uuid.UUID) (Transfer, error) {
	t, err := s.change(ctx, id, "complete",
		`UPDATE transfers SET state = 'completed', journal_id = $2, updated_at = now()
		 WHERE id = $1 AND state = 'pending' RETURNING `+columns,
		id, journalID)
	var finished *finishedError
	if errors.As(err, &finished) && finished.t.State == StateCompleted && finished.t.JournalID == journalID {
		return finished.t, nil // the same answer again
	}
	return t, err
}

// Fail marks pending transfer id failed, refused by the ledger for reason
// (its terror code). Failing it again for the same reason is fine; anything
// else on a finished transfer is an error.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, reason string) (Transfer, error) {
	t, err := s.change(ctx, id, "fail",
		`UPDATE transfers SET state = 'failed', failure_reason = $2, updated_at = now()
		 WHERE id = $1 AND state = 'pending' RETURNING `+columns,
		id, reason)
	var finished *finishedError
	if errors.As(err, &finished) && finished.t.State == StateFailed && finished.t.FailureReason == reason {
		return finished.t, nil // the same answer again
	}
	return t, err
}

// finishedError is a change asked of a transfer that's already completed or
// failed. It isn't a terror: it means the service itself has a bug, so the
// caller gets a 500.
type finishedError struct{ t Transfer }

func (e *finishedError) Error() string {
	return fmt.Sprintf("transfer %s is already %s, and a finished transfer never changes", e.t.ID, e.t.State)
}

// change runs an UPDATE that only matches a pending transfer. If it matches
// nothing, the transfer is missing (not_found) or finished (*finishedError).
func (s *Store) change(ctx context.Context, id uuid.UUID, what, sql string, args ...any) (Transfer, error) {
	t, err := scanTransfer(s.pool.QueryRow(ctx, sql, args...))
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, fmt.Errorf("%s transfer %s: %w", what, id, err)
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	return Transfer{}, fmt.Errorf("%s: %w", what, &finishedError{current})
}

func scanTransfer(row pgx.Row) (Transfer, error) {
	var (
		t         Transfer
		kind      string
		from      *uuid.UUID
		currency  string
		amount    int64
		state     string
		journalID *uuid.UUID
		reason    *string
	)
	if err := row.Scan(&t.ID, &t.IdempotencyKey, &kind, &from, &t.To, &currency, &amount, &state,
		&journalID, &reason, &t.Attempts, &t.NextAttemptAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return Transfer{}, err
	}
	c, err := money.ParseCurrency(currency)
	if err != nil { // the table and pkg/money have drifted apart: fail loudly rather than guess
		return Transfer{}, fmt.Errorf("stored transfer %s: %w", t.ID, err)
	}
	t.Kind, t.State, t.Amount = Kind(kind), State(state), money.New(amount, c)
	if from != nil {
		t.From = *from
	}
	if journalID != nil {
		t.JournalID = *journalID
	}
	if reason != nil {
		t.FailureReason = *reason
	}
	return t, nil
}
