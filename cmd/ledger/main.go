// Command ledger is the ledger service: the one record of every movement of
// money, kept by double-entry bookkeeping in its own Postgres database.
//
// Everything every database service does (config, logs, connecting and
// migrating before serving, probes, metrics, graceful shutdown) is db.Run.
// What's here is only what makes it the ledger: its migrations and its routes.
package main

import (
	"errors"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
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
func routes(router *typhon.Router, pool *pgxpool.Pool, getenv func(string) string) error {
	accountURL, err := baseURL(getenv("ACCOUNT_URL"))
	if err != nil {
		return err
	}
	ledger.RegisterRoutes(router, ledger.NewService(ledger.NewStore(pool), ledger.AccountService{BaseURL: accountURL}))
	return nil
}

// baseURL checks raw is an http(s) URL with a host and nothing after it, so
// a typo stops the service starting instead of failing every post.
func baseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("ACCOUNT_URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", errors.New("ACCOUNT_URL must be an http(s) URL with just a host, like http://account")
	}
	return u.Scheme + "://" + u.Host, nil
}
