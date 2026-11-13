package account

import (
	"context"
	"errors"
	"fmt"
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

// columns is selected by every query, in the order scanAccount reads them.
// pgx reads Postgres uuid columns straight into uuid.UUID.
const columns = "id, customer_id, holder_name, currency, status, created_at"

// Create opens a new account and returns it, with the ID and created time
// Postgres assigned. It validates req first, so a bad request returns
// ErrInvalid without touching the database.
func (s *Store) Create(ctx context.Context, req NewRequest) (Account, error) {
	req, err := req.Validate()
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES ($1, $2, $3) RETURNING "+columns,
		req.CustomerID, req.HolderName, req.Currency.Code(),
	)
	acc, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("create account: %w", err)
	}
	return acc, nil
}

// Get returns the account with id, or ErrNotFound. Parsing untrusted text
// into a uuid.UUID is the caller's job (the HTTP handler), so a malformed ID
// never gets this far.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Account, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+columns+" FROM accounts WHERE id = $1", id)
	acc, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return acc, nil
}

func scanAccount(row pgx.Row) (Account, error) {
	var (
		acc      Account
		currency string
		status   string
	)
	if err := row.Scan(&acc.ID, &acc.CustomerID, &acc.HolderName, &currency, &status, &acc.CreatedAt); err != nil {
		return Account{}, err
	}
	c, err := money.ParseCurrency(currency)
	if err != nil {
		// The table only allows supported currencies, so this means the table
		// and pkg/money have drifted apart. Fail loudly rather than guess.
		return Account{}, fmt.Errorf("stored account %s: %w", acc.ID, err)
	}
	acc.Currency = c
	acc.Status = Status(status)
	return acc, nil
}
