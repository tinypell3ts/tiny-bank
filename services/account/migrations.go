// Package account is the account service: customers' accounts, each holding
// money in one currency. Its Postgres schema lives in migrations/, applied by
// db.Migrate when the service starts (ADR-0005).
package account

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the service's goose migrations, ready for db.Migrate.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // can't happen: "migrations" is embedded above, so a failure is a build bug
	}
	return sub
}
