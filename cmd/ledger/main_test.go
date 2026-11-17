package main

import (
	"strings"
	"testing"

	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// TestConformance checks the ledger follows every rule services get from
// pkg/kit: health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	env := map[string]string{"ACCOUNT_URL": "http://account"}
	kittest.ConformanceOf(t, func(r *typhon.Router) {
		deps := db.Deps{Getenv: func(k string) string { return env[k] }, Metrics: prometheus.NewRegistry()}
		if err := routes(r, deps); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRoutesNeedsAnAccountURL(t *testing.T) {
	err := routes(&typhon.Router{}, db.Deps{Getenv: func(string) string { return "" }, Metrics: prometheus.NewRegistry()})
	if err == nil || !strings.Contains(err.Error(), "ACCOUNT_URL is required") {
		t.Fatalf("err = %v, want ACCOUNT_URL is required", err)
	}
}
