package ledger

import (
	"errors"
	"fmt"
	"testing"

	"github.com/monzo/terrors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestOutcomeOf(t *testing.T) {
	tests := []struct {
		name                string
		created             bool
		err                 error
		wantOutcome, reason string
	}{
		{"a new journal", true, nil, "posted", ""},
		{"a retry", false, nil, "retried", ""},
		{"insufficient funds, wrapped", false, fmt.Errorf("post journal: %w", errInsufficientFunds(alice)), "refused", "precondition_failed.insufficient_funds"},
		{"a bad journal", false, fmt.Errorf("post journal: %w", invalid("entries", "x")), "refused", "bad_request.entries"},
		{"an account that doesn't exist", false, errNoSuchAccount(alice), "refused", "not_found.account"},
		{"the account service failing", false, terrors.NewInternalWithCause(errors.New("down"), "x", nil, "account_service"), "failed", ""},
		{"a plain error", false, errors.New("database gone"), "failed", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, reason := outcomeOf(tt.created, tt.err)
			if outcome != tt.wantOutcome || reason != tt.reason {
				t.Errorf("outcomeOf = %s, %q; want %s, %q", outcome, reason, tt.wantOutcome, tt.reason)
			}
		})
	}
}

func TestRecordCountsMoneyMovedOnlyByNewJournals(t *testing.T) {
	m := newMetrics(prometheus.NewRegistry())
	req := NewJournal{IdempotencyKey: "t1", Entries: []Entry{{alice, gbp(-550)}, {bob, gbp(500)}, {carol, gbp(50)}}}
	j := Journal{Entries: req.Entries}

	m.record(req, j, true, nil)  // posted: £5.50 moved
	m.record(req, j, false, nil) // a retry: nothing more moved
	m.record(req, Journal{}, false, errInsufficientFunds(alice))

	if got := testutil.ToFloat64(m.moved.WithLabelValues("GBP")); got != 550 {
		t.Errorf("moved = %v, want 550: the positive entries of the one new journal", got)
	}
	for outcome, reason := range map[string]string{"posted": "", "retried": "", "refused": "precondition_failed.insufficient_funds"} {
		if got := testutil.ToFloat64(m.journals.WithLabelValues("GBP", outcome, reason)); got != 1 {
			t.Errorf("journals{%s,%s} = %v, want 1", outcome, reason, got)
		}
	}
	// A request too broken to have a currency is still counted.
	m.record(NewJournal{}, Journal{}, false, invalid("entries", "x"))
	if got := testutil.ToFloat64(m.journals.WithLabelValues("unknown", "refused", "bad_request.entries")); got != 1 {
		t.Errorf("journals{unknown} = %v, want 1", got)
	}
}
