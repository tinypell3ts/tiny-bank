package transaction

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEveryOutcomeHasAName(t *testing.T) {
	for o := posted; o <= unsure; o++ {
		if outcomeNames[o] == "" {
			t.Errorf("outcome %d has no metric label", o)
		}
	}
}

func TestPostsAreCountedByWhoAndOutcome(t *testing.T) {
	m := newMetrics(prometheus.NewRegistry())
	m.posted(byRequest, unsure, "")
	m.posted(byRequest, unsure, "")
	m.posted(byFinisher, refusedForGood, "precondition_failed.insufficient_funds")

	if got := testutil.ToFloat64(m.posts.WithLabelValues("request", "unsure", "")); got != 2 {
		t.Errorf("request unsure = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.posts.WithLabelValues("finisher", "refused", "precondition_failed.insufficient_funds")); got != 1 {
		t.Errorf("finisher refused = %v, want 1", got)
	}
	// Started at 0, so the first post shows in rate().
	if n := testutil.CollectAndCount(m.posts); n != 7 {
		t.Errorf("%d series, want 6 started at 0 plus the refusal", n)
	}
}

func TestSetPending(t *testing.T) {
	m := newMetrics(prometheus.NewRegistry())
	m.setPending(3, time.Now().Add(-time.Minute))
	if got := testutil.ToFloat64(m.pending); got != 3 {
		t.Errorf("pending = %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.oldestPending); got < 59 || got > 61 {
		t.Errorf("oldest = %vs, want about 60", got)
	}
	m.setPending(0, time.Time{})
	if testutil.ToFloat64(m.pending) != 0 || testutil.ToFloat64(m.oldestPending) != 0 {
		t.Error("with nothing pending, both should be 0")
	}
}
