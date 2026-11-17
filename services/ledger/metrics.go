package ledger

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/funding"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// metrics are the ledger's business metrics: what happens to money, beside
// the HTTP metrics every service gets from kit.Handler. Every label has a
// small, fixed set of values (AGENTS.md): never an account ID or an amount.
type metrics struct {
	// journals counts posts: ledger_journals_total{currency, outcome, reason}.
	journals *prometheus.CounterVec
	// moved counts the money moved by new journals, in minor units: the sum of
	// each journal's positive entries, so a £3 transfer adds 300.
	moved *prometheus.CounterVec
}

// The outcomes of a post.
const (
	outcomePosted  = "posted"  // a new journal: money moved
	outcomeRetried = "retried" // its key was already used: the first journal again, nothing moved
	outcomeRefused = "refused" // the caller's request broke a rule (a 4xx): nothing moved
	outcomeFailed  = "failed"  // something went wrong on our side (a 5xx): nothing moved
)

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		journals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_journals_total",
			Help: "Journal posts by currency and outcome (posted, retried, refused, failed), and for refusals the terror code.",
		}, []string{"currency", "outcome", "reason"}),
		moved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_moved_minor_units_total",
			Help: "Money moved by new journals, in the currency's minor unit (pence for GBP): the sum of each journal's positive entries.",
		}, []string{"currency"}),
	}
	reg.MustRegister(m.journals, m.moved)

	// Start every series we can name at 0. A counter that first appears
	// already at 300 hides that first increase: Prometheus has no earlier
	// sample to compare with, so rate() and increase() show 0 until the
	// second journal. Refusal reasons can't all be listed here, so those
	// series still start on first use.
	for currency := range funding.All() { // one funding account per supported currency
		m.moved.WithLabelValues(currency)
		for _, outcome := range []string{outcomePosted, outcomeRetried, outcomeFailed} {
			m.journals.WithLabelValues(currency, outcome, "")
		}
	}
	return m
}

// record counts one post of req, which returned j, created and err.
func (m *metrics) record(req NewJournal, j Journal, created bool, err error) {
	var c money.Currency
	if len(req.Entries) > 0 {
		c = req.Entries[0].Amount.Currency()
	}
	currency := currencyLabel(c)
	outcome, reason := outcomeOf(created, err)
	m.journals.WithLabelValues(currency, outcome, reason).Inc()
	if outcome == outcomePosted {
		var moved float64
		for _, e := range j.Entries {
			if a := e.Amount.Amount(); a > 0 {
				moved += float64(a)
			}
		}
		m.moved.WithLabelValues(currency).Add(moved)
	}
}

// refused counts a post turned away by the HTTP handler before it reached
// Service.Post. Its reason is the terror's code, which for these is one of a
// few fixed ones: kit.Decode's (bad_request.invalid_json, content_type,
// too_large, unreadable_body) and the handler's (bad_request.currency,
// bad_request.entries).
func (m *metrics) refused(c money.Currency, err error) {
	_, reason := outcomeOf(false, err)
	m.journals.WithLabelValues(currencyLabel(c), outcomeRefused, reason).Inc()
}

// currencyLabel is c's code, or "unknown" for a request too broken to have a
// supported currency.
func currencyLabel(c money.Currency) string {
	if c.Code() == "" {
		return "unknown"
	}
	return c.Code()
}

func outcomeOf(created bool, err error) (outcome, reason string) {
	if err == nil {
		if created {
			return outcomePosted, ""
		}
		return outcomeRetried, ""
	}
	if code, ok := kit.Refusal(err); ok {
		// The code is a small, fixed set: every code the ledger returns is a
		// constant in its own code, never built from what a caller sent, and
		// the account service's codes are turned into the ledger's own before
		// they get here (AccountService.Account).
		return outcomeRefused, code
	}
	return outcomeFailed, ""
}
