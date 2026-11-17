package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// accountColumns is selected by every accounts query, in the order
// scanBalance reads them.
const accountColumns = "id, kind, currency, balance"

// errAlreadyPosted rolls back a post whose idempotency key is already taken.
// It never leaves Post.
var errAlreadyPosted = errors.New("idempotency key already used")

// Post moves the money in req, all of it or none of it, and returns the
// journal. created is false when req is a retry of a journal already posted
// with the same key: nothing moves again, and the first journal is returned.
//
// Two posts that share an account run one after the other: each locks the
// accounts it touches (SELECT ... FOR UPDATE) until it commits. They lock in
// account ID order, so two posts can't each hold an account the other needs
// and wait for ever (a deadlock).
func (s *Store) Post(ctx context.Context, req NewJournal) (Journal, bool, error) {
	req, err := req.Validate()
	if err != nil {
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	var j Journal
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var postErr error
		j, postErr = post(ctx, tx, req)
		return postErr
	})
	if errors.Is(err, errAlreadyPosted) {
		j, found, err := s.posted(ctx, req)
		if err == nil && !found {
			// The key was taken a moment ago, and journals are never deleted.
			err = errors.New("journal for a taken idempotency key not found")
		}
		if err != nil {
			return Journal{}, false, fmt.Errorf("post journal: %w", err)
		}
		return j, false, nil
	}
	if err != nil {
		return Journal{}, false, fmt.Errorf("post journal: %w", err)
	}
	return j, true, nil
}

func post(ctx context.Context, tx pgx.Tx, req NewJournal) (Journal, error) {
	currency := req.Entries[0].Amount.Currency()
	j := Journal{IdempotencyKey: req.IdempotencyKey, Currency: currency, Entries: req.Entries}

	// Claim the key first. If another post holds it and hasn't committed,
	// this waits for it: if it commits, the key is taken (a retry); if it
	// rolls back, this post goes ahead. Either way the money moves once.
	err := tx.QueryRow(ctx,
		`INSERT INTO journals (idempotency_key, currency) VALUES ($1, $2)
		 ON CONFLICT (idempotency_key) DO NOTHING RETURNING id, created_at`,
		req.IdempotencyKey, currency.Code(),
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
			// at maxAmount, so a balance needs millions of huge journals to get
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
	balances, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Balance, error) { return scanBalance(row) })
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

// posted returns the journal already posted with req's idempotency key, if
// there is one (found). If the key was used for a different request, it
// returns errKeyReused instead. Both are sorted by account ID, so they compare
// entry by entry.
func (s *Store) posted(ctx context.Context, req NewJournal) (j Journal, found bool, err error) {
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `SELECT id FROM journals WHERE idempotency_key = $1`, req.IdempotencyKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Journal{}, false, nil
	}
	if err != nil {
		return Journal{}, false, fmt.Errorf("read journal for key: %w", err)
	}
	if j, err = s.Journal(ctx, id); err != nil {
		return Journal{}, false, err
	}
	if !slices.Equal(j.Entries, req.Entries) {
		return Journal{}, false, errKeyReused()
	}
	return j, true, nil
}

// Journal returns the journal with id and its entries, or a terrors not_found.
func (s *Store) Journal(ctx context.Context, id uuid.UUID) (Journal, error) {
	j := Journal{ID: id}
	var currency string
	err := s.pool.QueryRow(ctx, `SELECT idempotency_key, currency, created_at FROM journals WHERE id = $1`, id).
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
	rows, err := s.pool.Query(ctx, `SELECT account_id, amount FROM entries WHERE journal_id = $1 ORDER BY account_id`, id)
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
	b, err := scanBalance(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, errAccountNotFound()
	}
	if err != nil {
		return Balance{}, fmt.Errorf("get balance: %w", err)
	}
	return b, nil
}

func scanBalance(row pgx.Row) (Balance, error) {
	var (
		b        Balance
		kind     string
		currency string
		amount   int64
	)
	if err := row.Scan(&b.AccountID, &kind, &currency, &amount); err != nil {
		return Balance{}, err
	}
	c, err := money.ParseCurrency(currency)
	if err != nil { // the tables and pkg/money have drifted apart, as in Journal
		return Balance{}, fmt.Errorf("stored account %s: %w", b.AccountID, err)
	}
	b.Kind = Kind(kind)
	b.Amount = money.New(amount, c)
	return b, nil
}
