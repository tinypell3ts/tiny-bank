// Command transaction is the transaction service: it takes requests to move
// money, saves each as a transfer, and sees it through until the ledger has
// posted or refused it.
//
// Everything every database service does (config, logs, connecting and
// migrating before serving, probes, metrics, graceful shutdown) is db.Run.
// What's here is only what makes it the transaction service: its migrations
// and its routes.
package main

import (
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/services/transaction"
)

// run is the whole service. Tests run it with kittest.Start.
var run = db.Run("transaction", transaction.Migrations(), routes)

func main() { kit.Main("transaction", run) }

// routes is the transaction API, on the service's database.
func routes(router *typhon.Router, deps db.Deps) error {
	transaction.RegisterRoutes(router, transaction.NewStore(deps.Pool))
	return nil
}
