package transaction

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
