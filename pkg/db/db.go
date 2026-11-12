// Package db connects services to their Postgres database.
//
// Every service gets the same connection setup: a pool of connections built
// from DATABASE_URL (set by the shared Helm chart from the secret CloudNativePG
// generates), a short connect timeout so a missing database fails fast, and a
// check at startup so a bad URL or password is reported straight away, not on
// the first request.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// connectTimeout bounds each attempt to open a new connection, so an
// unreachable database fails in seconds rather than hanging a request or startup.
const connectTimeout = 5 * time.Second

// Open creates a connection pool for url, a postgres:// URL like DATABASE_URL,
// and checks that it can connect. The caller must Close the pool on shutdown.
//
// A pool keeps a few connections open and lends them out per query, because
// opening a new Postgres connection for every request is slow.
//
// Errors are safe to log: pgx replaces the password with "xxxxx", and
// TestOpenFailsWithoutLeakingThePassword makes sure it stays that way.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("open database: invalid DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// NewWithConfig connects lazily, so check now that the database is really there.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return pool, nil
}
