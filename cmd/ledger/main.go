// Command ledger is the ledger service: the one record of every movement of
// money, kept by double-entry bookkeeping in its own Postgres database.
//
// Everything every database service does (config, logs, connecting and
// migrating before serving, probes, metrics, graceful shutdown) is db.Run.
// What's here is only what makes it the ledger: its migrations and its routes.
package main

import (
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/services/ledger"
)

// run is the whole service. Tests run it with kittest.Start.
var run = db.Run("ledger", ledger.Migrations(), routes)

func main() { kit.Main("ledger", run) }

// routes is the ledger API, on the service's database. ACCOUNT_URL, required,
// is where the account service is (http://account on the cluster, set in
// deploy/values/ledger.yaml): the ledger checks every customer account with
// it before moving money.
func routes(router *typhon.Router, deps db.Deps) error {
	accountURL, err := kit.ServiceURL(deps.Getenv, "ACCOUNT_URL")
	if err != nil {
		return err
	}
	accounts := ledger.AccountService{BaseURL: accountURL}
	ledger.RegisterRoutes(router, ledger.NewService(ledger.NewStore(deps.Pool), accounts, deps.Metrics))
	return nil
}
