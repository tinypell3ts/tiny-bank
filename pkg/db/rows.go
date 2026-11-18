package db

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Columns is the column list of row struct T, from its fields' `db` tags, as
// a SELECT or RETURNING list: "id, kind, currency". A store builds its
// queries from it and reads them back with pgx.RowToStructByName into T, so
// the columns it asks for and the fields they land in are one list, written
// once. Every exported field must have a `db` tag (or `db:"-"` to leave it
// out); one without is a mistake in the code, so Columns panics, which a
// store's package-level var turns into a failure at startup and in tests.
func Columns[T any]() string {
	t := reflect.TypeFor[T]()
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		switch tag := f.Tag.Get("db"); tag {
		case "":
			panic(fmt.Sprintf("db.Columns: %s.%s has no db tag", t.Name(), f.Name))
		case "-":
		default:
			names = append(names, tag)
		}
	}
	return strings.Join(names, ", ")
}

// Querier runs a query: a pool, or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// QueryOne runs sql on q and reads its one row with scan (which reads by
// column name: pgx.RowToStructByName). No row is pgx.ErrNoRows, as with
// QueryRow, so callers check for it the same way; a query that fails
// part way, like an UPDATE a trigger refuses, returns that error.
func QueryOne[T any](ctx context.Context, q Querier, scan pgx.RowToFunc[T], sql string, args ...any) (T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		var zero T
		return zero, err
	}
	return pgx.CollectOneRow(rows, scan)
}
