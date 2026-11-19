package main

import (
	"context"
	"strings"
	"testing"

	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// TestConformance checks the transaction service follows every rule services
// get from pkg/kit: health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	env := map[string]string{"LEDGER_URL": "http://ledger", "KAFKA_BROKERS": "kafka:9092"}
	kittest.ConformanceOf(t, func(r *typhon.Router) {
		deps := db.Deps{Getenv: func(k string) string { return env[k] }, Metrics: prometheus.NewRegistry(), Background: func(func(context.Context)) {}}
		if err := routes(r, deps); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRoutesStartTheFinisherAndTheRelay: without the finisher, a transfer
// the ledger didn't answer in time would stay pending for ever; without the
// relay, events would wait in the outbox for ever.
func TestRoutesStartTheFinisherAndTheRelay(t *testing.T) {
	env := map[string]string{"LEDGER_URL": "http://ledger", "KAFKA_BROKERS": "kafka:9092"}
	started := 0
	deps := db.Deps{
		Getenv:     func(k string) string { return env[k] },
		Metrics:    prometheus.NewRegistry(),
		Background: func(func(context.Context)) { started++ },
	}
	if err := routes(&typhon.Router{}, deps); err != nil || started != 2 {
		t.Fatalf("routes = %v, %d background jobs; want the finisher and the relay", err, started)
	}
}

// TestRoutesNeedKafka: without KAFKA_BROKERS the service refuses to start,
// rather than saving events it can never send.
func TestRoutesNeedKafka(t *testing.T) {
	deps := db.Deps{
		Getenv:     func(k string) string { return map[string]string{"LEDGER_URL": "http://ledger"}[k] },
		Metrics:    prometheus.NewRegistry(),
		Background: func(func(context.Context)) {},
	}
	if err := routes(&typhon.Router{}, deps); err == nil || !strings.Contains(err.Error(), "KAFKA_BROKERS") {
		t.Errorf("routes = %v, want an error naming KAFKA_BROKERS", err)
	}
}

func TestRoutesNeedsALedgerURL(t *testing.T) {
	err := routes(&typhon.Router{}, db.Deps{Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "LEDGER_URL is required") {
		t.Fatalf("err = %v, want LEDGER_URL is required", err)
	}
}
