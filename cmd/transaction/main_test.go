package main

import (
	"context"
	"strings"
	"testing"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// TestConformance checks the transaction service follows every rule services
// get from pkg/kit: health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	env := map[string]string{"LEDGER_URL": "http://ledger"}
	kittest.ConformanceOf(t, func(r *typhon.Router) {
		deps := db.Deps{Getenv: func(k string) string { return env[k] }, Background: func(func(context.Context)) {}}
		if err := routes(r, deps); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRoutesStartTheFinisher: without it, a transfer the ledger didn't answer
// in time would stay pending for ever.
func TestRoutesStartTheFinisher(t *testing.T) {
	started := 0
	deps := db.Deps{
		Getenv:     func(string) string { return "http://ledger" },
		Background: func(func(context.Context)) { started++ },
	}
	if err := routes(&typhon.Router{}, deps); err != nil || started != 1 {
		t.Fatalf("routes = %v, %d background jobs; want the finisher started once", err, started)
	}
}

func TestRoutesNeedsALedgerURL(t *testing.T) {
	err := routes(&typhon.Router{}, db.Deps{Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "LEDGER_URL is required") {
		t.Fatalf("err = %v, want LEDGER_URL is required", err)
	}
}
