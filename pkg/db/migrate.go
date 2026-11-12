package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migrate applies any migrations in fsys that the database doesn't have yet,
// in order, and returns the files it applied. It's safe to call on every
// startup: with nothing new to apply it does nothing.
//
// fsys holds goose SQL files at its root, named <version>_<name>.sql, like
// 00001_create_accounts.sql. Services embed theirs:
//
//	//go:embed migrations/*.sql
//	var migrations embed.FS
//	sub, _ := fs.Sub(migrations, "migrations")
//
// Several pods may start at once, so Migrate holds a Postgres advisory lock
// while it runs. Only one pod migrates; the others wait (checking every
// second, for up to 5 minutes), then find nothing left to do. Each migration
// runs in its own transaction, so a failing one leaves no half-applied change.
// See ADR-0005.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// goose works with database/sql, so give it a *sql.DB backed by the same pool.
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }() // closes the wrapper only, not the pool

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		// On failure goose reports the migrations that did succeed inside the error.
		var partial *goose.PartialError
		if errors.As(err, &partial) {
			results = partial.Applied
		}
	}
	applied := make([]string, 0, len(results))
	for _, r := range results {
		applied = append(applied, r.Source.Path)
	}
	if err != nil {
		return applied, fmt.Errorf("migrate: %w", err)
	}
	return applied, nil
}
