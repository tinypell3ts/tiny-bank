package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/monzo/terrors"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// Store posts journals and reads balances in the service's Postgres database.
//
// Every query passes values as parameters ($1, $2), never by building SQL
// from strings, so input can't change what a query does (SQL injection).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store using pool. The schema must already be migrated.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// accountColumns is selected by every accounts query: balanceRow's columns,
// from its db tags, so the columns asked for and the fields they're read
// into are one list.
var accountColumns = db.Columns[balanceRow]()

// errAlreadyPosted rolls back a post whose idempotency key is already taken.
// It never leaves Post.
var errAlreadyPosted = errors.New("idempotency key already used")

// postValid moves the money in req, all of it or none of it, and returns the
// journal. created is false when req's key already has an answer: the
// journal posted with it (nothing moves again), or the refusal it got, as
// the same error.
//
// req must already be validated (NewJournal.Validate): its entries sorted and
// adding up to zero. It's unexported, so Service.Post, which validates and
// checks the accounts first, is the only way to post. Nothing can wire the
// Store in on its own and skip the checks.
//
// Two posts that share an account run one after the other: each locks the
// accounts it touches (SELECT ... FOR UPDATE) until it commits. They lock in
// account ID order, so two posts can't each hold an account the other needs
// and wait for ever (a deadlock).
func (s *Store) postValid(ctx context.Context, req NewJournal) (Journal, bool, error) {
	return s.decide(ctx, req, func(tx pgx.Tx) (Journal, error) { return insertJournal(ctx, tx, req) })
}

// refuse records refusal as the answer to req's key, unless the key already
// has one, which is returned instead. The Service calls it when an account
// check refuses req, so a later request with the same key can't post it.
func (s *Store) refuse(ctx context.Context, req NewJournal, refusal error) (Journal, bool, error) {
	return s.decide(ctx, req, func(pgx.Tx) (Journal, error) { return Journal{}, refusal })
}

// decide gives req's idempotency key its one final answer. Under a lock on
// the key, so posts with the same key run one at a time: if the key already
// has an answer (a journal, or a refusal), that's returned, created false.
// Otherwise post runs, in a savepoint; if it refuses req (keptRefusal), its
// writes are undone and the refusal is saved against the key instead, in the
// same transaction. So once a request with a key is refused, a later one
// can't post it, and once it's posted, a later one gets the journal. Any
// other error, such as the database failing, saves nothing: the key stays
// free for a retry.
func (s *Store) decide(ctx context.Context, req NewJournal, post func(pgx.Tx) (Journal, error)) (Journal, bool, error) {
	var (
		j       Journal
		created bool
		answer  error // the refusal the caller gets, if any
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// A transaction lock on a hash of the key: held until commit, and
		// taken before any account lock, so the order is always the same.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, req.IdempotencyKey); err != nil {
			return fmt.Errorf("lock idempotency key: %w", err)
		}
		prev, found, prevErr := answered(ctx, tx, req)
		if found {
			j, answer = prev, prevErr
			return nil
		}
		if prevErr != nil {
			return prevErr
		}

		var postErr error
		spErr := pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { // a savepoint: postErr rolls back only post's writes
			j, postErr = post(sp)
			return postErr
		})
		terr, kept := keptRefusal(postErr)
		switch {
		case kept:
			j, answer = Journal{}, postErr
			return insertRefusal(ctx, tx, req, terr)
		case postErr != nil:
			return postErr
		case spErr != nil: // the savepoint itself failed
			return spErr
		}
		created = true
		return nil
	})
	if errors.Is(err, errAlreadyPosted) {
		// Can't happen under the key lock, but if it does, the key's answer
		// is the journal posted with it.
		prev, found, prevErr := answered(ctx, s.pool, req)
		if !found && prevErr == nil {
			prevErr = errors.New("journal for a taken idempotency key not found")
		}
		return prev, false, wrap(prevErr)
	}
	if err != nil {
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	if answer != nil {
		return Journal{}, false, wrap(answer)
	}
	return j, created, nil
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("post journal: %w", err)
}

// keptRefusal reports whether err refuses the request for good, as a 4xx
// (kit.Refusal), and returns its terror to save against the key. A reused key
// isn't kept: it's about another request, not this one.
func keptRefusal(err error) (*terrors.Error, bool) {
	code, ok := kit.Refusal(err)
	var terr *terrors.Error
	if !ok || code == "precondition_failed.idempotency_key" || !errors.As(err, &terr) {
		return nil, false
	}
	return terr, true
}

func insertRefusal(ctx context.Context, tx pgx.Tx, req NewJournal, terr *terrors.Error) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO refusals (idempotency_key, request, code, message, request_id) VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
		req.IdempotencyKey, fingerprint(req), terr.Code, terr.Message, kit.RequestIDFrom(ctx)); err != nil {
		return fmt.Errorf("record refusal: %w", err)
	}
	return nil
}

// fingerprint identifies req's movement of money: a sha256 of its entries,
// which Validate has sorted, so the same request always has the same one.
func fingerprint(req NewJournal) string {
	h := sha256.New()
	for _, e := range req.Entries {
		_, _ = fmt.Fprintf(h, "%s %s %d\n", e.AccountID, e.Amount.Currency().Code(), e.Amount.Amount()) // a hash.Hash's Write never fails
	}
	return hex.EncodeToString(h.Sum(nil))
}

// querier runs queries: the pool, or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// insertJournal claims req's idempotency key, locks and checks its accounts,
// and writes the journal, inside postValid's transaction.
func insertJournal(ctx context.Context, tx pgx.Tx, req NewJournal) (Journal, error) {
	currency := req.Entries[0].Amount.Currency()
	j := Journal{IdempotencyKey: req.IdempotencyKey, Currency: currency, Entries: req.Entries}

	// Claim the key first. If another post holds it and hasn't committed,
	// this waits for it: if it commits, the key is taken (a retry); if it
	// rolls back, this post goes ahead. Either way the money moves once.
	err := tx.QueryRow(ctx,
		`INSERT INTO journals (idempotency_key, currency, request_id) VALUES ($1, $2, NULLIF($3, ''))
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING id, created_at`,
		req.IdempotencyKey, currency.Code(), kit.RequestIDFrom(ctx),
	).Scan(&j.ID, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Journal{}, errAlreadyPosted
	}
	if err != nil {
		return Journal{}, fmt.Errorf("insert journal: %w", err)
	}

	balances, err := lockAccounts(ctx, tx, req.Entries)
	if err != nil {
		return Journal{}, err
	}

	ids := make([]uuid.UUID, len(req.Entries))
	amounts := make([]int64, len(req.Entries))
	after := make([]int64, len(req.Entries))
	for i, e := range req.Entries {
		b := balances[i]
		if b.Amount.Currency() != e.Amount.Currency() {
			return Journal{}, errWrongCurrency(e.AccountID, b.Amount.Currency())
		}
		sum, err := b.Amount.Add(e.Amount)
		if err != nil {
			// The currencies match, so this is an overflow. Entries are capped
			// at money.MaxAmount, so a balance needs millions of huge journals to get
			// here: a fault, not one caller's mistake.
			return Journal{}, fmt.Errorf("account %s: %w", e.AccountID, err)
		}
		if b.Kind == KindCustomer && sum.Amount() < 0 {
			return Journal{}, errInsufficientFunds(e.AccountID)
		}
		ids[i], amounts[i], after[i] = e.AccountID, e.Amount.Amount(), sum.Amount()
	}

	// Every check passed, so write the new balances and the entries. Both go
	// in one round trip, so the locks are held for as short a time as possible.
	batch := &pgx.Batch{}
	batch.Queue(`UPDATE accounts AS a SET balance = v.balance
		FROM unnest($1::uuid[], $2::bigint[]) AS v (id, balance) WHERE a.id = v.id`, ids, after)
	batch.Queue(`INSERT INTO entries (journal_id, account_id, amount)
		SELECT $1, v.id, v.amount FROM unnest($2::uuid[], $3::bigint[]) AS v (id, amount)`, j.ID, ids, amounts)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return Journal{}, fmt.Errorf("write journal %s: %w", j.ID, err)
	}
	return j, nil
}

// lockAccounts locks the accounts entries touch, creating a customer account
// for any the ledger hasn't seen, and returns their balances in the same
// order as entries. Both statements go in one round trip.
//
// The rows are created and locked in account ID order (ORDER BY id), so two
// posts that share accounts always take their locks in the same order and
// can't deadlock: one waits for the other instead.
func lockAccounts(ctx context.Context, tx pgx.Tx, entries []Entry) ([]Balance, error) {
	ids := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		ids[i] = e.AccountID
	}
	batch := &pgx.Batch{}
	// A customer account's row is created by its first journal, in the
	// journal's currency.
	batch.Queue(`INSERT INTO accounts (id, kind, currency)
		SELECT id, 'customer', $2 FROM unnest($1::uuid[]) AS id ORDER BY id
		ON CONFLICT (id) DO NOTHING`, ids, entries[0].Amount.Currency().Code())
	batch.Queue(`SELECT `+accountColumns+` FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)

	results := tx.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }() // any error was already returned by Exec or Query
	if _, err := results.Exec(); err != nil {
		return nil, fmt.Errorf("create accounts: %w", err)
	}
	rows, err := results.Query()
	if err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	balances, err := pgx.CollectRows(rows, scanBalance)
	if err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	// entries and the rows are both sorted by account ID, and the insert above
	// creates any missing row, so row i is entry i's account. Check rather
	// than trust it, as a mismatch would apply the wrong balance.
	if len(balances) != len(entries) {
		return nil, fmt.Errorf("lock accounts: locked %d rows for %d entries", len(balances), len(entries))
	}
	for i, b := range balances {
		if b.AccountID != entries[i].AccountID {
			return nil, fmt.Errorf("lock accounts: row %d is account %s, want %s", i, b.AccountID, entries[i].AccountID)
		}
	}
	return balances, nil
}

// answered returns the answer req's idempotency key already has, if any
// (found): the journal posted with it, or the refusal it got, as that terror.
// If the key was used for a different request, found is true and the error
// is errKeyReused. Journals are compared entry by entry (both are sorted by
// account ID), and refusals by the request's fingerprint.
func answered(ctx context.Context, q querier, req NewJournal) (j Journal, found bool, err error) {
	var (
		id                     *uuid.UUID
		request, code, message *string
	)
	err = q.QueryRow(ctx,
		`SELECT j.id, r.request, r.code, r.message FROM (SELECT $1::text AS key) k
		 LEFT JOIN journals j ON j.idempotency_key = k.key
		 LEFT JOIN refusals r ON r.idempotency_key = k.key`, req.IdempotencyKey).Scan(&id, &request, &code, &message)
	if err != nil {
		return Journal{}, false, fmt.Errorf("read answer for key: %w", err)
	}
	switch {
	case id != nil:
		if j, err = journal(ctx, q, *id); err != nil {
			return Journal{}, false, err
		}
		if !slices.Equal(j.Entries, req.Entries) {
			return Journal{}, true, errKeyReused()
		}
		return j, true, nil
	case request != nil:
		if *request != fingerprint(req) {
			return Journal{}, true, errKeyReused()
		}
		return Journal{}, true, &terrors.Error{Code: *code, Message: *message, Params: map[string]string{}}
	}
	return Journal{}, false, nil
}

// Journal returns the journal with id and its entries, or a terrors not_found.
func (s *Store) Journal(ctx context.Context, id uuid.UUID) (Journal, error) {
	return journal(ctx, s.pool, id)
}

func journal(ctx context.Context, q querier, id uuid.UUID) (Journal, error) {
	j := Journal{ID: id}
	var currency string
	err := q.QueryRow(ctx, `SELECT idempotency_key, currency, created_at FROM journals WHERE id = $1`, id).
		Scan(&j.IdempotencyKey, &currency, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Journal{}, errJournalNotFound()
	}
	if err != nil {
		return Journal{}, fmt.Errorf("get journal: %w", err)
	}
	if j.Currency, err = money.ParseCurrency(currency); err != nil {
		// The tables only allow supported currencies, so this means they and
		// pkg/money have drifted apart. Fail loudly rather than guess.
		return Journal{}, fmt.Errorf("get journal %s: %w", id, err)
	}

	// Postgres orders uuids byte by byte, like uuid.UUID.Compare, so this is
	// the order Validate sorted them into.
	rows, err := q.Query(ctx, `SELECT account_id, amount FROM entries WHERE journal_id = $1 ORDER BY account_id`, id)
	if err != nil {
		return Journal{}, fmt.Errorf("get journal %s entries: %w", id, err)
	}
	j.Entries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var (
			e      Entry
			amount int64
		)
		err := row.Scan(&e.AccountID, &amount)
		e.Amount = money.New(amount, j.Currency)
		return e, err
	})
	if err != nil {
		return Journal{}, fmt.Errorf("get journal %s entries: %w", id, err)
	}
	return j, nil
}

// Balance returns how much account id holds, or a terrors not_found if no
// journal has touched it yet.
func (s *Store) Balance(ctx context.Context, id uuid.UUID) (Balance, error) {
	b, err := db.QueryOne(ctx, s.pool, scanBalance, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, errAccountNotFound()
	}
	if err != nil {
		return Balance{}, fmt.Errorf("get balance: %w", err)
	}
	return b, nil
}

// balanceRow is a row of accounts, read by column name
// (pgx.RowToStructByName), so the order of accountColumns doesn't matter and
// a missing or extra column is an error, never a silent mix-up.
type balanceRow struct {
	ID       uuid.UUID `db:"id"`
	Kind     string    `db:"kind"`
	Currency string    `db:"currency"`
	Amount   int64     `db:"balance"` // in minor units
}

func scanBalance(row pgx.CollectableRow) (Balance, error) {
	r, err := pgx.RowToStructByName[balanceRow](row)
	if err != nil {
		return Balance{}, err
	}
	c, err := money.ParseCurrency(r.Currency)
	if err != nil { // the tables and pkg/money have drifted apart, as in Journal
		return Balance{}, fmt.Errorf("stored account %s: %w", r.ID, err)
	}
	return Balance{AccountID: r.ID, Kind: Kind(r.Kind), Amount: money.New(r.Amount, c)}, nil
}
