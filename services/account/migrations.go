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
// It's a function, not an exported variable, so no other package can swap
// them out.
func Migrations() fs.FS { return migrationFiles }
