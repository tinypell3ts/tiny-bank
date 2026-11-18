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
	journal_id, failure_reason, attempts, next_attempt_at, created_at, updated_at, lease, trace_id, request_id`

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
		`INSERT INTO transfers (idempotency_key, kind, from_account, to_account, currency, amount, trace_id, request_id)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''))
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING `+columns,
		req.IdempotencyKey, req.Kind, from, req.To, req.Amount.Currency().Code(), req.Amount.Amount(), req.TraceID, req.RequestID))
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

// Claim makes the caller the only one attempting pending transfer id, if
// it's due: it gives the transfer a new random Lease and pushes its
// next_attempt_at forward by lease, so nobody else can claim it meanwhile.
// ok is false when it isn't due (someone else is attempting it, or it's
// waiting to be retried) or is already finished: the transfer is then
// returned as it stands.
func (s *Store) Claim(ctx context.Context, id uuid.UUID, lease time.Duration) (Transfer, bool, error) {
	t, err := scanTransfer(s.pool.QueryRow(ctx,
		`UPDATE transfers SET lease = gen_random_uuid(), next_attempt_at = now() + $2::interval, updated_at = now()
		 WHERE id = $1 AND state = 'pending' AND next_attempt_at <= now() RETURNING `+columns,
		id, lease))
	if errors.Is(err, pgx.ErrNoRows) {
		t, err = s.Get(ctx, id)
		return t, false, err
	}
	if err != nil {
		return Transfer{}, false, fmt.Errorf("claim transfer %s: %w", id, err)
	}
	return t, true, nil
}

// StartAttempt records that pending transfer id is about to be sent to the
// ledger by the holder of lease: it counts the attempt, and sets when it's
// next due if this one doesn't finish it (after retryAfter). Recording it
// first means that if the service dies mid-attempt, the transfer is still
// picked up and tried again. If someone else holds the lease now, it's an
// errLeaseLost, and the caller must not post.
func (s *Store) StartAttempt(ctx context.Context, id, lease uuid.UUID, retryAfter time.Duration) (Transfer, error) {
	return s.change(ctx, id, "start attempt on",
		`UPDATE transfers SET attempts = attempts + 1, next_attempt_at = now() + $2::interval, updated_at = now()
		 WHERE id = $1 AND state = 'pending' AND lease = $3 RETURNING `+columns,
		id, retryAfter, lease)
}

// ClaimDue claims up to limit pending transfers that are due to be tried
// again, oldest first, and returns them. Claiming one pushes its
// next_attempt_at forward by lease, so no other finisher (another pod) picks
// it up meanwhile; FOR UPDATE SKIP LOCKED means two finishers claiming at the
// same moment skip each other's rows instead of waiting for them, so each
// gets a different set. If the finisher dies holding a claim, the transfer is
// due again once the lease runs out.
func (s *Store) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]Transfer, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE transfers SET lease = gen_random_uuid(), next_attempt_at = now() + $2::interval, updated_at = now()
		 WHERE id IN (SELECT id FROM transfers WHERE state = 'pending' AND next_attempt_at <= now()
		              ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		 RETURNING `+columns,
		limit, lease)
	if err != nil {
		return nil, fmt.Errorf("claim due transfers: %w", err)
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Transfer, error) { return scanTransfer(row) })
	if err != nil {
		return nil, fmt.Errorf("claim due transfers: %w", err)
	}
	return due, nil
}

// Pending returns how many transfers are pending and when the oldest of them
// was created (zero if none is).
func (s *Store) Pending(ctx context.Context) (int, time.Time, error) {
	var (
		count  int
		oldest *time.Time
	)
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(created_at) FROM transfers WHERE state = 'pending'`).Scan(&count, &oldest); err != nil {
		return 0, time.Time{}, fmt.Errorf("count pending transfers: %w", err)
	}
	if oldest == nil {
		return count, time.Time{}, nil
	}
	return count, *oldest, nil
}

// Complete marks pending transfer id completed, posted by the ledger as
// journal journalID. Completing it again with the same journal is fine (an
// answer that arrived twice); anything else on a finished transfer is an
// error, since a finished transfer never changes.
func (s *Store) Complete(ctx context.Context, id, journalID uuid.UUID) (Transfer, error) {
	return s.finish(ctx, id, "complete",
		`UPDATE transfers SET state = 'completed', journal_id = $2, updated_at = now()
		 WHERE id = $1 AND state = 'pending' RETURNING `+columns,
		func(t Transfer) bool { return t.State == StateCompleted && t.JournalID == journalID },
		id, journalID)
}

// Fail marks pending transfer id failed, refused by the ledger for reason
// (its terror code), if the caller still holds lease. A refusal only counts
// from the lease holder: an attempt that lost its lease may have been
// overtaken by one that moved the money, so its refusal is an errLeaseLost,
// and the transfer stays as it is. (Complete needs no lease: once the ledger
// has posted the journal, completed is the truth whoever reports it.)
// Failing it again for the same reason is fine; anything else on a finished
// transfer is an error.
func (s *Store) Fail(ctx context.Context, id, lease uuid.UUID, reason string) (Transfer, error) {
	return s.finish(ctx, id, "fail",
		`UPDATE transfers SET state = 'failed', failure_reason = $2, updated_at = now()
		 WHERE id = $1 AND state = 'pending' AND lease = $3 RETURNING `+columns,
		func(t Transfer) bool { return t.State == StateFailed && t.FailureReason == reason },
		id, reason, lease)
}

// finish moves a pending transfer to a final state with sql. If it's already
// finished and sameAnswer says it was finished this same way, that's the same
// answer arriving twice, so it's returned as if this call had done it.
// Finished any other way, it's a *finishedError: a bug, since a finished
// transfer never changes.
func (s *Store) finish(ctx context.Context, id uuid.UUID, what, sql string, sameAnswer func(Transfer) bool, args ...any) (Transfer, error) {
	t, err := s.change(ctx, id, what, sql, args...)
	var finished *finishedError
	if errors.As(err, &finished) && sameAnswer(finished.t) {
		return finished.t, nil
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

// errLeaseLost is a change asked by an attempt whose lease someone else now
// holds. The transfer is someone else's to finish.
var errLeaseLost = errors.New("someone else is attempting this transfer now")

// change runs an UPDATE that only matches a pending transfer (and, for some,
// its lease). If it matches nothing, the transfer is missing (not_found),
// finished (*finishedError), or pending under another lease (errLeaseLost,
// with the transfer as it now stands).
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
	if current.State == StatePending {
		return current, fmt.Errorf("%s transfer %s: %w", what, id, errLeaseLost)
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
		lease     *uuid.UUID
		traceID   *string
		requestID *string
	)
	if err := row.Scan(&t.ID, &t.IdempotencyKey, &kind, &from, &t.To, &currency, &amount, &state,
		&journalID, &reason, &t.Attempts, &t.NextAttemptAt, &t.CreatedAt, &t.UpdatedAt, &lease, &traceID, &requestID); err != nil {
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
	if lease != nil {
		t.Lease = *lease
	}
	if traceID != nil {
		t.TraceID = *traceID
	}
	if requestID != nil {
		t.RequestID = *requestID
	}
	return t, nil
}
