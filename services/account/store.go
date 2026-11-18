package account

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

// Store saves and loads accounts in the service's Postgres database.
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

// columns is selected by every query. scanAccount reads them by name, so
// their order doesn't matter, but each must be a field of accountRow. pgx
// reads Postgres uuid columns straight into uuid.UUID.
const columns = "id, customer_id, holder_name, currency, status, created_at"

// Create opens a new account and returns it, with the ID and created time
// Postgres assigned. It validates req first, so a bad request returns a
// terrors bad_request without touching the database.
func (s *Store) Create(ctx context.Context, req NewRequest) (Account, error) {
	req, err := req.Validate()
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	acc, err := s.queryAccount(ctx,
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES ($1, $2, $3) RETURNING "+columns,
		req.CustomerID, req.HolderName, req.Currency.Code(),
	)
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	return acc, nil
}

// Get returns the account with id, or a terrors not_found. Parsing untrusted text
// into a uuid.UUID is the caller's job (the HTTP handler), so a malformed ID
// never gets this far.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Account, error) {
	acc, err := s.queryAccount(ctx, "SELECT "+columns+" FROM accounts WHERE id = $1", id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, errNotFound()
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return acc, nil
}

// accountRow is a row of accounts, read by column name
// (pgx.RowToStructByName): the order of the columns doesn't matter, and a
// column with no field here, or a field with no column, is an error rather
// than a value quietly landing in the wrong field.
type accountRow struct {
	ID         uuid.UUID `db:"id"`
	CustomerID uuid.UUID `db:"customer_id"`
	HolderName string    `db:"holder_name"`
	Currency   string    `db:"currency"`
	Status     string    `db:"status"`
	CreatedAt  time.Time `db:"created_at"`
}

// queryAccount runs sql, which must return exactly the columns, and reads its
// one row into an Account. No row is pgx.ErrNoRows, as with QueryRow.
func (s *Store) queryAccount(ctx context.Context, sql string, args ...any) (Account, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return Account{}, err
	}
	return pgx.CollectOneRow(rows, scanAccount)
}

func scanAccount(row pgx.CollectableRow) (Account, error) {
	r, err := pgx.RowToStructByName[accountRow](row)
	if err != nil {
		return Account{}, err
	}
	c, err := money.ParseCurrency(r.Currency)
	if err != nil {
		// The table only allows supported currencies, so this means the table
		// and pkg/money have drifted apart. Fail loudly rather than guess.
		return Account{}, fmt.Errorf("stored account %s: %w", r.ID, err)
	}
	return Account{ID: r.ID, CustomerID: r.CustomerID, HolderName: r.HolderName, Currency: c, Status: Status(r.Status), CreatedAt: r.CreatedAt}, nil
}
