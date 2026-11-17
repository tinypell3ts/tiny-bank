package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	good := config{rate: 5, duration: time.Second, accounts: 2, fund: 1, maxAmount: 1, retryShare: 0.1, maxInFlight: 1}
	if err := good.validate(); err != nil {
		t.Fatalf("a good config: %v", err)
	}
	bad := map[string]func(*config){
		"no rate":                               func(c *config) { c.rate = 0 },
		"no duration":                           func(c *config) { c.duration = 0 },
		"one account":                           func(c *config) { c.accounts = 1 },
		"no funding":                            func(c *config) { c.fund = 0 },
		"no amount":                             func(c *config) { c.maxAmount = 0 },
		"retries above 1":                       func(c *config) { c.retryShare = 1.5 },
		"no requests at once":                   func(c *config) { c.maxInFlight = 0 },
		"rate too fast":                         func(c *config) { c.rate = maxRate + 1 },
		"a rate that rounds to a zero interval": func(c *config) { c.rate = 2e9 },
		"rate not a number":                     func(c *config) { c.rate = math.NaN() },
		"rate infinite":                         func(c *config) { c.rate = math.Inf(1) },
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

func TestGeneratorRetriesOnlyPostedJournals(t *testing.T) {
	g := newGenerator([]string{"a", "b"}, 500, 1, "run") // every request is a retry once one has been posted
	refused := g.next()
	if again := g.next(); again.IdempotencyKey == refused.IdempotencyKey {
		t.Fatal("resent a journal that was never posted: a retry should be of one that moved money, so it answers 200")
	}
	posted := g.next()
	g.posted(posted)
	for range 50 {
		if again := g.next(); again.IdempotencyKey != posted.IdempotencyKey || again.Entries[1].Amount != posted.Entries[1].Amount {
			t.Fatalf("retry = %+v, want the posted journal again, unchanged", again)
		}
	}
}

func TestGeneratorRetryPoolStaysBounded(t *testing.T) {
	g := newGenerator([]string{"a", "b"}, 500, 0, "run")
	var last journal
	for range 3 * retryPoolSize {
		last = g.next()
		g.posted(last)
	}
	if g.poolLen != retryPoolSize {
		t.Fatalf("pool holds %d journals, want at most %d", g.poolLen, retryPoolSize)
	}
	found := false
	for _, j := range g.pool {
		found = found || j.IdempotencyKey == last.IdempotencyKey
	}
	if !found {
		t.Error("the newest posted journal isn't in the pool: it should replace the oldest")
	}
}

func TestStatsKeepABoundedFairSample(t *testing.T) {
	s := &stats{}
	n := 3 * latencySamples
	for i := range n {
		s.add(http.StatusCreated, time.Duration(i+1)*time.Microsecond, nil)
	}
	if len(s.latencies) != latencySamples || s.answered != n {
		t.Fatalf("kept %d latencies of %d answered; want %d kept", len(s.latencies), s.answered, latencySamples)
	}
	// The latencies rose steadily, so a fair sample's median is near the
	// middle of the whole run, not near its start.
	mid := time.Duration(n/2) * time.Microsecond
	if got := percentile(s.latencies, 50); got < mid*9/10 || got > mid*11/10 {
		t.Errorf("sampled p50 = %s, want about %s", got, mid)
	}
	if !strings.Contains(s.summary(time.Second), fmt.Sprintf("Sent %d requests", n)) {
		t.Error("the summary must count every request, not only the sampled ones")
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

// TestRunRetriesNeverResendARefusedKey: with every post refused, nothing may
// be resent. A refused key isn't kept by the ledger, so a resend could move
// money.
func TestRunRetriesNeverResendARefusedKey(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	acc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("a0000000-0000-4000-8000-%012d", time.Now().UnixNano()%1e12)})
	}))
	led := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j journal
		_ = json.NewDecoder(r.Body).Decode(&j)
		if strings.HasPrefix(j.IdempotencyKey, "fund-") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		mu.Lock()
		seen[j.IdempotencyKey]++
		mu.Unlock()
		w.WriteHeader(http.StatusPreconditionFailed) // insufficient funds, every time
	}))
	t.Cleanup(acc.Close)
	t.Cleanup(led.Close)

	cfg := config{rate: 200, duration: 300 * time.Millisecond, accounts: 2, fund: 1, maxAmount: 100,
		retryShare: 0.9, maxInFlight: 10, accountURL: acc.URL, ledgerURL: led.URL}
	if err := run(t.Context(), cfg, http.DefaultClient, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	for key, n := range seen {
		if n > 1 {
			t.Fatalf("refused key %s was sent %d times", key, n)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no journals were sent")
	}
}

// TestSetupGivesUpOnAServiceThatNeverAnswers: setup requests have a deadline
// too, so a hung service stops the tool with an error instead of for ever.
func TestSetupGivesUpOnAServiceThatNeverAnswers(t *testing.T) {
	old := requestTimeout
	requestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { requestTimeout = old })
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	t.Cleanup(func() { close(hang); srv.Close() })

	cfg := config{rate: 1, duration: time.Second, accounts: 2, fund: 1, maxAmount: 1, maxInFlight: 1, accountURL: srv.URL, ledgerURL: srv.URL}
	start := time.Now()
	if err := run(t.Context(), cfg, http.DefaultClient, &strings.Builder{}); err == nil {
		t.Fatal("setup against a hung service succeeded")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("setup took %v to give up, want about the %v request timeout", took, requestTimeout)
	}
}

// TestClientReusesConnections: the tool's own client must reuse a pool, or
// at high rates it runs the machine out of local ports and measures that.
func TestClientReusesConnections(t *testing.T) {
	var dialled atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dialled.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	const maxInFlight = 50
	client := newClient(maxInFlight)
	for range 5 {
		var wg sync.WaitGroup
		for range maxInFlight {
			wg.Go(func() {
				if _, _, err := postJSON(t.Context(), client, srv.URL, journal{}); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	}
	if n := dialled.Load(); n > maxInFlight+1 {
		t.Errorf("%d requests opened %d connections; want at most %d, reused", 5*maxInFlight, n, maxInFlight+1)
	}
}
