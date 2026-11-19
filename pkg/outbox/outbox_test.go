package outbox

import (
	"errors"
	"testing"
	"time"
)

// TestPauseBacksOffAfterFailures: after a batch that sent something, go
// again now; after one that found nothing, poll; after failures in a row,
// double the wait from 400ms up to maxBackoff, and back to polling as soon as
// a batch works.
func TestPauseBacksOffAfterFailures(t *testing.T) {
	failed := errors.New("UNKNOWN_TOPIC_OR_PARTITION")
	type batch struct {
		sent int
		err  error
	}
	batches := []batch{
		{0, nil},    // nothing to send
		{0, failed}, // first failure
		{0, failed}, //
		{0, failed}, //
		{0, failed}, //
		{0, failed}, // capped
		{0, nil},    // working again, nothing to send
		{3, failed}, // some sent, one refused: go again now
		{0, failed}, // a failure right after a send
		{5, nil},    // sent
	}
	want := []time.Duration{
		pollInterval,
		400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, maxBackoff,
		pollInterval,
		0,
		400 * time.Millisecond,
		0,
	}
	var wait time.Duration
	for i, b := range batches {
		if wait = pause(wait, b.sent, b.err); wait != want[i] {
			t.Errorf("batch %d (sent %d, err %v): pause = %v, want %v", i, b.sent, b.err, wait, want[i])
		}
	}
}
