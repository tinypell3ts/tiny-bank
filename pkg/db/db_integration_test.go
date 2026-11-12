//go:build integration

// Integration tests run against a real Postgres in Docker, started by
// testcontainers. Run them with `make test-integration`; plain `go test` and
// `make check` skip this file because of the build tag above.
package db

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// postgresImage matches the Postgres version CloudNativePG runs on the cluster
// (ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie). Tests use the official
// image because testcontainers relies on its startup script to set a password.
const postgresImage = "postgres:18.6-trixie"

// startPostgres runs a throwaway Postgres for one test and returns its URL.
// The container is removed when the test ends.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("app"),
		postgres.WithUsername("app"),
		postgres.WithPassword(secret),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func TestOpenConnectsAndQueries(t *testing.T) {
	dsn := startPostgres(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	var user, database string
	var major int
	err = pool.QueryRow(ctx,
		"select current_user, current_database(), current_setting('server_version_num')::int / 10000",
	).Scan(&user, &database, &major)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if user != "app" || database != "app" || major != 18 {
		t.Errorf("connected as %s to %s on Postgres %d, want app, app, 18", user, database, major)
	}
}

func TestOpenRejectsWrongPassword(t *testing.T) {
	dsn := startPostgres(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("app", "wrong-"+secret)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := Open(ctx, u.String())
	if err == nil {
		pool.Close()
		t.Fatal("Open succeeded with the wrong password")
	}
	if !strings.Contains(err.Error(), "password authentication failed") {
		t.Errorf("err = %q, want a password authentication failure", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("err leaks the password: %q", err)
	}
}
