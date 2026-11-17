// Package dbtest gives integration tests a throwaway Postgres.
//
// Most tests want Database or Migrated: a fresh database of their own, on a
// Postgres shared by every test in the package. Starting Postgres takes
// seconds, and creating a database milliseconds. StartPostgres runs a whole
// server for one test, for the few that need one (their own password, or to
// take the database away).
//
// Only test files import it (tagged //go:build integration), so its Docker
// and testcontainers dependencies never end up in a service binary.
package dbtest

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
)

// Image matches the Postgres version CloudNativePG runs on the cluster
// (ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie). Tests use the official
// image because testcontainers relies on its startup script to set a password.
const Image = "postgres:18.6-trixie"

// Password is user "app"'s password on the shared Postgres behind Database,
// so a test can check it never leaks into logs or errors.
const Password = "s3cret"

// StartPostgres runs a Postgres with user and database "app" (like
// CloudNativePG's) and the given password, and returns its postgres:// URL.
// The container is removed when the test ends. It needs Docker running.
func StartPostgres(t *testing.T, password string) string {
	t.Helper()
	ctx := t.Context()
	ctr, err := runPostgres(ctx, password)
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

func runPostgres(ctx context.Context, password string) (*postgres.PostgresContainer, error) {
	return postgres.Run(ctx, Image,
		postgres.WithDatabase("app"),
		postgres.WithUsername("app"),
		postgres.WithPassword(password),
		postgres.BasicWaitStrategies(),
	)
}

// shared is the Postgres behind Database, started by the first test that
// asks for one. It lives as long as the test binary (one package's tests):
// testcontainers' reaper removes it once the binary exits.
var shared struct {
	once  sync.Once
	url   *url.URL      // the "app" database
	admin *pgxpool.Pool // on the "app" database, to create the others
	err   error
	n     atomic.Int64 // databases created so far, to name the next one
}

func startShared() {
	// Not the test's context: the server outlives the test that starts it.
	ctx := context.Background()
	ctr, err := runPostgres(ctx, Password)
	if err != nil {
		shared.err = fmt.Errorf("start postgres: %w", err)
		return
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		shared.err = fmt.Errorf("connection string: %w", err)
		return
	}
	if shared.url, err = url.Parse(dsn); err != nil {
		shared.err = fmt.Errorf("parse url: %w", err)
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	shared.admin, shared.err = db.Open(openCtx, dsn)
}

// Database creates an empty database for one test, owned by user "app" (with
// Password), and returns its postgres:// URL. It needs Docker running.
func Database(t *testing.T) string {
	t.Helper()
	shared.once.Do(startShared)
	if shared.err != nil {
		t.Fatalf("shared postgres: %v", shared.err)
	}
	name := fmt.Sprintf("test_%d", shared.n.Add(1))
	// CREATE DATABASE can't take its name as a $1 parameter, so the name is
	// quoted as an identifier. It's always ours, never a caller's.
	if _, err := shared.admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	u := *shared.url
	u.Path = "/" + name
	return u.String()
}

// Migrated creates a database for one test with Database, applies a
// service's migrations to it with db.Migrate (the same code the service runs
// at startup), and returns a pool on it. The pool is closed when the test
// ends.
//
// pkg/db's own tests can't use it, as this package imports pkg/db.
func Migrated(t *testing.T, migrations fs.FS) *pgxpool.Pool {
	t.Helper()
	// Get the database first: starting Postgres (and pulling its image) can be
	// slow, and shouldn't use up the deadline for connecting and migrating.
	url := Database(t)
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
