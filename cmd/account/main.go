// Command account is the account service: it opens and looks up customers'
// accounts, stored in its own Postgres database.
//
// Everything every database service does (config, logs, connecting and
// migrating before serving, probes, metrics, graceful shutdown) is db.Run.
// What's here is only what makes it the account service: its migrations and
// its routes.
package main

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/services/account"
)

// run is the whole service. Tests run it with kittest.Start.
var run = db.Run("account", account.Migrations(), routes)

func main() { kit.Main("account", run) }

// routes is the account API, on the service's database. It has no settings
// of its own.
func routes(router *typhon.Router, pool *pgxpool.Pool, _ func(string) string) error {
	account.RegisterRoutes(router, account.NewStore(pool))
	return nil
}
