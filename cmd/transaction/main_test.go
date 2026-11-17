package main

import (
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
		if err := routes(r, db.Deps{Getenv: func(k string) string { return env[k] }}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRoutesNeedsALedgerURL(t *testing.T) {
	err := routes(&typhon.Router{}, db.Deps{Getenv: func(string) string { return "" }})
	if err == nil || !strings.Contains(err.Error(), "LEDGER_URL is required") {
		t.Fatalf("err = %v, want LEDGER_URL is required", err)
	}
}
