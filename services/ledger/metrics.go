package ledger

import (
	"errors"
	"strings"

	"github.com/monzo/terrors"
	"github.com/prometheus/client_golang/prometheus"
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

// callersFault are the terror classes that mean the request was refused (a
// 4xx), not that the ledger failed.
var callersFault = map[string]bool{
	terrors.ErrBadRequest:         true,
	terrors.ErrNotFound:           true,
	terrors.ErrPreconditionFailed: true,
}

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
	return m
}

// record counts one post of req, which returned j, created and err.
func (m *metrics) record(req NewJournal, j Journal, created bool, err error) {
	currency := "unknown" // a request too broken to have one
	if len(req.Entries) > 0 && req.Entries[0].Amount.Currency().Code() != "" {
		currency = req.Entries[0].Amount.Currency().Code()
	}
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

func outcomeOf(created bool, err error) (outcome, reason string) {
	if err == nil {
		if created {
			return outcomePosted, ""
		}
		return outcomeRetried, ""
	}
	var terr *terrors.Error
	if errors.As(err, &terr) && callersFault[strings.SplitN(terr.Code, ".", 2)[0]] {
		// The code is a small, fixed set: every code the ledger returns is a
		// constant in its own code, never built from what a caller sent, and
		// the account service's codes are turned into the ledger's own before
		// they get here (AccountService.Account).
		return outcomeRefused, terr.Code
	}
	return outcomeFailed, ""
}
