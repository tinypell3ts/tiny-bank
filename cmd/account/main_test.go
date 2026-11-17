package main

import (
	"testing"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// TestConformance checks the account service follows every rule services get
// from pkg/kit: health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	kittest.ConformanceOf(t, func(r *typhon.Router) { _ = routes(r, db.Deps{}) })
}
