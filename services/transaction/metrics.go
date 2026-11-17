package transaction

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the transaction service's business metrics, beside the HTTP
// metrics every service gets from kit.Handler. Every label has a small, fixed
// set of values (AGENTS.md): never a transfer ID, an account or an amount.
type metrics struct {
	// posts counts calls to the ledger:
	// transaction_ledger_posts_total{by, outcome, reason}.
	posts *prometheus.CounterVec
	// pending and oldestPending say whether transfers are getting stuck: how
	// many are pending, and how long the oldest has been. The finisher sets
	// them each pass.
	pending       prometheus.Gauge
	oldestPending prometheus.Gauge
}

// Who tried the ledger: a request, or the background finisher.
const (
	byRequest  = "request"
	byFinisher = "finisher"
)

// outcomeNames are the outcomes as metric labels.
var outcomeNames = map[outcome]string{
	posted:         "posted",
	refusedForGood: "refused",
	keyReused:      "key_reused",
	unsure:         "unsure",
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		posts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "transaction_ledger_posts_total",
			Help: "Posts to the ledger, by who tried (request, finisher) and outcome (posted, refused, key_reused, unsure), and for refusals the ledger's terror code.",
		}, []string{"by", "outcome", "reason"}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "transaction_pending_transfers",
			Help: "Transfers still pending: saved, but not yet completed or failed by the ledger.",
		}),
		oldestPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "transaction_oldest_pending_seconds",
			Help: "How long ago the oldest pending transfer was created, or 0 if none is pending.",
		}),
	}
	reg.MustRegister(m.posts, m.pending, m.oldestPending)

	// Start every series we can name at 0, so the first increase shows in
	// rate() (as the ledger's metrics do). Refusal reasons start on first use.
	for _, by := range []string{byRequest, byFinisher} {
		for _, o := range []outcome{posted, keyReused, unsure} {
			m.posts.WithLabelValues(by, outcomeNames[o], "")
		}
	}
	return m
}

// posted counts one post to the ledger by by, which had outcome o and, for a
// refusal, reason: one of the ledger's own fixed terror codes.
func (m *metrics) posted(by string, o outcome, reason string) {
	m.posts.WithLabelValues(by, outcomeNames[o], reason).Inc()
}

// setPending records how many transfers are pending and when the oldest was
// created (zero if none is).
func (m *metrics) setPending(count int, oldest time.Time) {
	m.pending.Set(float64(count))
	if oldest.IsZero() {
		m.oldestPending.Set(0)
		return
	}
	m.oldestPending.Set(time.Since(oldest).Seconds())
}
