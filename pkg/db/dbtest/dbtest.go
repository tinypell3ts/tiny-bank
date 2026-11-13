// Package dbtest starts a throwaway Postgres for integration tests.
//
// Only test files import it (tagged //go:build integration), so its Docker
// and testcontainers dependencies never end up in a service binary.
package dbtest

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
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
