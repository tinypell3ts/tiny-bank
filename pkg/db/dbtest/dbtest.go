// Package dbtest starts a throwaway Postgres for integration tests.
//
// Only test files import it (tagged //go:build integration), so its Docker
// and testcontainers dependencies never end up in a service binary.
package dbtest

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
)

// Image matches the Postgres version CloudNativePG runs on the cluster
// (ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie). Tests use the official
// image because testcontainers relies on its startup script to set a password.
const Image = "postgres:18.6-trixie"

// StartPostgres runs a Postgres with user and database "app" (like
// CloudNativePG's) and the given password, and returns its postgres:// URL.
// The container is removed when the test ends. It needs Docker running.
func StartPostgres(t *testing.T, password string) string {
	t.Helper()
	ctx := t.Context()
	ctr, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("app"),
		postgres.WithUsername("app"),
		postgres.WithPassword(password),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return url
}

// Migrated starts a throwaway Postgres, applies a service's migrations to it
// with db.Migrate (the same code the service runs at startup), and returns a
// pool on it. The pool is closed when the test ends.
//
// pkg/db's own tests can't use it, as this package imports pkg/db.
func Migrated(t *testing.T, migrations fs.FS) *pgxpool.Pool {
	t.Helper()
	// Start Postgres first: pulling the image can be slow, and shouldn't use up
	// the deadline for connecting and migrating.
	url := StartPostgres(t, "s3cret")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool, migrations); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
