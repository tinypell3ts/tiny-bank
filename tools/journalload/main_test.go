package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	good := config{rate: 5, duration: time.Second, accounts: 2, fund: 1, maxAmount: 1, retryShare: 0.1, maxInFlight: 1}
	if err := good.validate(); err != nil {
		t.Fatalf("a good config: %v", err)
	}
	bad := map[string]func(*config){
		"no rate":             func(c *config) { c.rate = 0 },
		"no duration":         func(c *config) { c.duration = 0 },
		"one account":         func(c *config) { c.accounts = 1 },
		"no funding":          func(c *config) { c.fund = 0 },
		"no amount":           func(c *config) { c.maxAmount = 0 },
		"retries above 1":     func(c *config) { c.retryShare = 1.5 },
		"no requests at once": func(c *config) { c.maxInFlight = 0 },
	}
	for name, change := range bad {
		c := good
		change(&c)
		if c.validate() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestGeneratorMakesValidTransfers(t *testing.T) {
	g := newGenerator([]string{"a", "b", "c"}, 500, 0, "run")
	keys := map[string]bool{}
	for range 1000 {
		j := g.next()
		if len(j.Entries) != 2 || j.Currency != "GBP" {
			t.Fatalf("journal = %+v, want a GBP transfer", j)
		}
		from, to := j.Entries[0], j.Entries[1]
		if from.AccountID == to.AccountID {
			t.Fatalf("a transfer from %s to itself", from.AccountID)
		}
		if from.Amount+to.Amount != 0 || to.Amount < 1 || to.Amount > 500 {
			t.Fatalf("entries %d and %d: want them to add up to zero, each 1 to 500", from.Amount, to.Amount)
		}
		if keys[j.IdempotencyKey] {
			t.Fatalf("key %s used twice with retries off", j.IdempotencyKey)
		}
		keys[j.IdempotencyKey] = true
	}
}

func TestGeneratorRetriesResendEarlierJournals(t *testing.T) {
	g := newGenerator([]string{"a", "b"}, 500, 1, "run") // after the first, every request is a retry
	first := g.next()
	for range 50 {
		if again := g.next(); again.IdempotencyKey != first.IdempotencyKey || again.Entries[1].Amount != first.Entries[1].Amount {
			t.Fatalf("retry = %+v, want the first journal again, unchanged", again)
		}
	}
}

func TestPercentile(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	for p, want := range map[int]time.Duration{50: 50 * time.Millisecond, 95: 95 * time.Millisecond, 99: 99 * time.Millisecond, 100: 100 * time.Millisecond} {
		if got := percentile(ds, p); got != want {
			t.Errorf("p%d = %s, want %s", p, got, want)
		}
	}
	if got := percentile([]time.Duration{7 * time.Millisecond}, 50); got != 7*time.Millisecond {
		t.Errorf("one sample: p50 = %s", got)
	}
}

// fakeServices stand in for the account service and the ledger: accounts get
// numbered IDs, and the ledger answers 201 for a new key and 200 for one
// it has seen.
type fakeServices struct {
	mu       sync.Mutex
	accounts int
	keys     map[string]bool
	posted   int
	retried  int
}

func (f *fakeServices) start(t *testing.T) (accountURL, ledgerURL string) {
	t.Helper()
	f.keys = map[string]bool{}
	acc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.accounts++
		id := fmt.Sprintf("a0000000-0000-4000-8000-%012d", f.accounts)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	}))
	led := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j journal
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.keys[j.IdempotencyKey] {
			f.retried++
			w.WriteHeader(http.StatusOK)
			return
		}
		f.keys[j.IdempotencyKey] = true
		f.posted++
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(acc.Close)
	t.Cleanup(led.Close)
	return acc.URL, led.URL
}

func TestRunPostsAtTheRate(t *testing.T) {
	f := &fakeServices{}
	accountURL, ledgerURL := f.start(t)
	cfg := config{rate: 100, duration: 500 * time.Millisecond, accounts: 4, fund: 100_000, maxAmount: 1000,
		retryShare: 0.2, maxInFlight: 10, accountURL: accountURL, ledgerURL: ledgerURL}

	var out strings.Builder
	if err := run(t.Context(), cfg, http.DefaultClient, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.accounts != 4 {
		t.Errorf("opened %d accounts, want 4", f.accounts)
	}
	// 4 deposits, then about 100 a second for half a second. Timers are
	// loose on a busy machine, so allow plenty either way.
	sent := f.posted + f.retried - 4
	if sent < 25 || sent > 60 {
		t.Errorf("sent %d journals in 0.5s at 100 a second, want about 50", sent)
	}
	if f.retried == 0 {
		t.Error("no retries, want about 20% of requests to resend an earlier one")
	}
	for _, want := range []string{"Sent ", "201 posted", "200 retried", "Latency: p50"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary is missing %q:\n%s", want, out.String())
		}
	}
}

func TestLoadSkipsInsteadOfQueueing(t *testing.T) {
	cfg := config{rate: 200, duration: 200 * time.Millisecond, maxInFlight: 1}
	release := make(chan struct{})
	s := &stats{}
	var calls int
	var mu sync.Mutex
	go func() { time.Sleep(300 * time.Millisecond); close(release) }()
	load(t.Context(), cfg, func(context.Context) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release // the one request in flight never finishes during the run
	}, s)
	if calls != 1 || s.skipped < 10 {
		t.Errorf("%d calls, %d skipped; want 1 call and the rest skipped, not queued", calls, s.skipped)
	}
}

func TestRunFailsClearlyWhenTheServicesArentRunning(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	cfg := config{rate: 1, duration: time.Second, accounts: 2, fund: 1, maxAmount: 1, maxInFlight: 1, accountURL: srv.URL, ledgerURL: srv.URL}
	err := run(t.Context(), cfg, http.DefaultClient, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "is make dev running?") {
		t.Fatalf("err = %v, want a hint to start make dev", err)
	}
}
