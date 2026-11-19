package outbox

import (
	"errors"
	"testing"
	"time"
)

// TestNextWaitBacksOffAfterFailures: each failure in a row doubles the wait,
// up to maxBackoff, and a batch that works goes back to pollInterval.
func TestNextWaitBacksOffAfterFailures(t *testing.T) {
	failed := errors.New("UNKNOWN_TOPIC_OR_PARTITION")
	var waits []time.Duration
	wait := pollInterval
	for range 7 {
		wait = nextWait(wait, failed)
		waits = append(waits, wait)
	}
	want := []time.Duration{400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond,
		3200 * time.Millisecond, maxBackoff, maxBackoff, maxBackoff}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits after failures = %v, want %v", waits, want)
		}
	}
	if got := nextWait(maxBackoff, nil); got != pollInterval {
		t.Errorf("after a batch that worked, wait = %v, want %v", got, pollInterval)
	}
}
