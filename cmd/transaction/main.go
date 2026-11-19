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
	"context"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kafka"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/outbox"
	"github.com/tinypell3ts/tiny-bank/services/transaction"
)

// run is the whole service. Tests run it with kittest.Start.
var run = db.Run("transaction", transaction.Migrations(), routes)

func main() { kit.Main("transaction", run) }

// routes is the transaction API, on the service's database, plus two things
// in the background: the finisher, which finishes what requests left
// pending, and the outbox relay, which sends the service's events to Kafka.
// Both settings are required, and set in deploy/values/transaction.yaml:
// LEDGER_URL is where the ledger is (http://ledger on the cluster), as every
// transfer is posted to it, and KAFKA_BROKERS is where Kafka is.
func routes(router *typhon.Router, deps db.Deps) error {
	ledgerURL, err := kit.ServiceURL(deps.Getenv, "LEDGER_URL")
	if err != nil {
		return err
	}
	brokers, err := kafka.Brokers(deps.Getenv)
	if err != nil {
		return err
	}
	producer, err := kafka.NewProducer(brokers) // connects on the first event, not here
	if err != nil {
		return err
	}
	service := transaction.NewService(transaction.NewStore(deps.Pool), transaction.LedgerService{BaseURL: ledgerURL}, deps.Metrics)
	transaction.RegisterRoutes(router, service)
	deps.Background(func(ctx context.Context) { service.RunFinisher(ctx, deps.Logger) })
	relay := outbox.NewRelay(deps.Pool, producer, deps.Logger, deps.Metrics)
	deps.Background(func(ctx context.Context) {
		defer producer.Close()
		relay.Run(ctx)
	})
	return nil
}
