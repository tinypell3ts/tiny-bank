// Command journalload posts random journals to the ledger at a steady rate,
// to see the ledger, its metrics and its dashboard under load.
//
//	go run ./tools/journalload -rate 5 -duration 1m
//
// It opens accounts with the account service, funds each from the GBP
// funding account, then posts random transfers between them. Some resend an
// earlier request with the same idempotency key (a retry, which moves
// nothing), and as balances drift some are refused for insufficient funds,
// so every outcome on the dashboard gets traffic. It prints a summary at the
// end.
//
// It talks to the services the way any caller does, over HTTP and JSON, so
// it needs them running: `make dev` forwards the account service to
// localhost:8081 and the ledger to localhost:8082.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// gbpFunding is the ledger's GBP funding account, where money from outside
// the bank comes from (ledger.FundingAccount).
const gbpFunding = "00000000-0000-4000-8000-000000000826"

type config struct {
	rate        float64       // journals per second
	duration    time.Duration // how long to keep posting
	accounts    int           // accounts to open and move money between
	fund        int64         // pence deposited into each account first
	maxAmount   int64         // the largest transfer, in pence
	retryShare  float64       // share of requests that resend an earlier one
	maxInFlight int           // requests allowed at once; more are skipped
	accountURL  string
	ledgerURL   string
}

func main() {
	cfg := config{}
	flag.Float64Var(&cfg.rate, "rate", 5, "journals to post per second")
	flag.DurationVar(&cfg.duration, "duration", time.Minute, "how long to keep posting, like 30s or 5m")
	flag.IntVar(&cfg.accounts, "accounts", 10, "accounts to open and move money between")
	flag.Int64Var(&cfg.fund, "fund", 20_000, "pence to deposit into each account first (20000 is £200: low enough that some transfers are refused)")
	flag.Int64Var(&cfg.maxAmount, "max-amount", 10_000, "the largest transfer, in pence (10000 is £100)")
	flag.Float64Var(&cfg.retryShare, "retries", 0.1, "share of requests that resend an earlier one, from 0 to 1")
	flag.IntVar(&cfg.maxInFlight, "max-in-flight", 100, "requests allowed at once; more are counted as skipped")
	flag.StringVar(&cfg.accountURL, "account-url", "http://localhost:8081", "the account service")
	flag.StringVar(&cfg.ledgerURL, "ledger-url", "http://localhost:8082", "the ledger")
	flag.Parse()

	// Ctrl-C stops early and still prints the summary.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, http.DefaultClient, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "journalload:", err)
		os.Exit(1)
	}
}

func (c config) validate() error {
	switch {
	case c.rate <= 0:
		return errors.New("-rate must be more than 0")
	case c.duration <= 0:
		return errors.New("-duration must be more than 0")
	case c.accounts < 2:
		return errors.New("-accounts must be at least 2, to move money between")
	case c.fund <= 0 || c.maxAmount <= 0:
		return errors.New("-fund and -max-amount must be more than 0")
	case c.retryShare < 0 || c.retryShare > 1:
		return errors.New("-retries must be from 0 to 1")
	case c.maxInFlight < 1:
		return errors.New("-max-in-flight must be at least 1")
	}
	return nil
}

func run(ctx context.Context, cfg config, client *http.Client, out io.Writer) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	runID := strconv.FormatInt(time.Now().UnixNano(), 36) // keeps this run's keys apart from earlier runs'

	if _, err := fmt.Fprintf(out, "Opening %d accounts and funding each with %s...\n", cfg.accounts, pounds(cfg.fund)); err != nil {
		return err
	}
	accounts, err := openAccounts(ctx, client, cfg, runID)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "Posting %g journals a second for %s (Ctrl-C to stop early)...\n", cfg.rate, cfg.duration); err != nil {
		return err
	}
	gen := newGenerator(accounts, cfg.maxAmount, cfg.retryShare, runID)
	stats := &stats{}
	start := time.Now()
	load(ctx, cfg, func(ctx context.Context) {
		body := gen.next()
		status, took, err := postJSON(ctx, client, cfg.ledgerURL+"/journals", body)
		stats.add(status, took, err)
	}, stats)

	_, err = io.WriteString(out, stats.summary(time.Since(start)))
	return err
}

// load calls post cfg.rate times a second for cfg.duration, or until ctx
// ends, each in its own goroutine, with at most cfg.maxInFlight at once. A
// tick that finds them all busy is skipped, not queued: queueing would hide
// that the ledger can't keep up, and then fire a burst when it recovered.
func load(ctx context.Context, cfg config, post func(context.Context), s *stats) {
	ctx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()
	ticker := time.NewTicker(time.Duration(float64(time.Second) / cfg.rate))
	defer ticker.Stop()

	slots := make(chan struct{}, cfg.maxInFlight)
	var wg sync.WaitGroup
	defer wg.Wait() // let the requests already sent finish
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case slots <- struct{}{}:
				wg.Go(func() {
					defer func() { <-slots }()
					// A request already sent finishes even if the run ends, so
					// its answer is counted. It gets its own short deadline.
					reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					post(reqCtx)
				})
			default:
				s.skip()
			}
		}
	}
}

// openAccounts opens cfg.accounts GBP accounts and deposits cfg.fund into
// each, from the GBP funding account.
func openAccounts(ctx context.Context, client *http.Client, cfg config, runID string) ([]string, error) {
	ids := make([]string, cfg.accounts)
	for i := range ids {
		status, body, err := postJSONBody(ctx, client, cfg.accountURL+"/accounts", map[string]string{
			"customer_id": "c0ffee00-0000-4000-8000-000000000001",
			"holder_name": fmt.Sprintf("Load test %d", i+1),
			"currency":    "GBP",
		})
		if err != nil {
			return nil, fmt.Errorf("open an account at %s (is make dev running?): %w", cfg.accountURL, err)
		}
		if status != http.StatusCreated {
			return nil, fmt.Errorf("open an account at %s: status %d: %s", cfg.accountURL, status, body)
		}
		var acc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &acc); err != nil || acc.ID == "" {
			return nil, fmt.Errorf("open an account: unexpected answer %s", body)
		}
		ids[i] = acc.ID

		status, body, err = postJSONBody(ctx, client, cfg.ledgerURL+"/journals",
			transfer("fund-"+runID+"-"+acc.ID, gbpFunding, acc.ID, cfg.fund))
		if err != nil {
			return nil, fmt.Errorf("fund account %s at %s: %w", acc.ID, cfg.ledgerURL, err)
		}
		if status != http.StatusCreated {
			return nil, fmt.Errorf("fund account %s at %s: status %d: %s", acc.ID, cfg.ledgerURL, status, body)
		}
	}
	return ids, nil
}

// journal is a POST /journals body (proto/ledger/v1).
type journal struct {
	IdempotencyKey string  `json:"idempotency_key"`
	Currency       string  `json:"currency"`
	Entries        []entry `json:"entries"`
}

type entry struct {
	AccountID string `json:"account_id"`
	Amount    int64  `json:"amount"`
}

func transfer(key, from, to string, pence int64) journal {
	return journal{IdempotencyKey: key, Currency: "GBP", Entries: []entry{{from, -pence}, {to, pence}}}
}

// generator makes the journals to post: random transfers between two
// different accounts, and now and then a resend of an earlier one.
type generator struct {
	mu         sync.Mutex
	accounts   []string
	maxAmount  int64
	retryShare float64
	runID      string
	n          int
	sent       []journal // every new journal so far, for retries to pick from
}

func newGenerator(accounts []string, maxAmount int64, retryShare float64, runID string) *generator {
	return &generator{accounts: accounts, maxAmount: maxAmount, retryShare: retryShare, runID: runID}
}

func (g *generator) next() journal {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) > 0 && float64(randN(1_000_000)) < g.retryShare*1_000_000 {
		return g.sent[randN(len(g.sent))] // the same request again: a retry
	}
	from := randN(len(g.accounts))
	to := randN(len(g.accounts) - 1)
	if to >= from {
		to++ // never the same account twice in one journal
	}
	g.n++
	j := transfer(fmt.Sprintf("load-%s-%d", g.runID, g.n), g.accounts[from], g.accounts[to], 1+int64(randN(int(g.maxAmount))))
	g.sent = append(g.sent, j)
	return j
}

// randN returns a random int from 0 to n-1. It uses crypto/rand, which is as
// easy as math/rand here and needs no explaining to a security linter.
func randN(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err) // crypto/rand doesn't fail on any supported platform
	}
	return int(v.Int64())
}

func postJSON(ctx context.Context, client *http.Client, url string, v any) (status int, took time.Duration, err error) {
	start := time.Now()
	status, _, err = postJSONBody(ctx, client, url, v)
	return status, time.Since(start), err
}

func postJSONBody(ctx context.Context, client *http.Client, url string, v any) (int, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// stats collects what happened to every request.
type stats struct {
	mu        sync.Mutex
	byStatus  map[int]int
	errors    int
	skipped   int
	latencies []time.Duration
}

func (s *stats) add(status int, took time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.errors++
		return
	}
	if s.byStatus == nil {
		s.byStatus = map[int]int{}
	}
	s.byStatus[status]++
	s.latencies = append(s.latencies, took)
}

func (s *stats) skip() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped++
}

// summary describes the run, after it has ended.
func (s *stats) summary(elapsed time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	answered := len(s.latencies)
	sent := answered + s.errors
	fmt.Fprintf(&b, "\nSent %d requests in %s: %.1f a second.\n", sent, elapsed.Round(time.Millisecond), float64(sent)/elapsed.Seconds())
	rows := []struct {
		label string
		n     int
	}{
		{"201 posted (money moved)", s.byStatus[http.StatusCreated]},
		{"200 retried (same key, nothing moved)", s.byStatus[http.StatusOK]},
		{"412 refused (insufficient funds and other rules)", s.byStatus[http.StatusPreconditionFailed]},
		{"other 4xx", s.count(400, 499) - s.byStatus[http.StatusPreconditionFailed]},
		{"5xx", s.count(500, 599)},
		{"no answer (connection or timeout)", s.errors},
		{"skipped (max-in-flight reached)", s.skipped},
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-50s %d\n", r.label, r.n)
	}
	if answered > 0 {
		fmt.Fprintf(&b, "Latency: p50 %s, p95 %s, p99 %s\n",
			percentile(s.latencies, 50), percentile(s.latencies, 95), percentile(s.latencies, 99))
	}
	return b.String()
}

// count adds up the requests answered with a status from lo to hi.
func (s *stats) count(lo, hi int) int {
	n := 0
	for status, c := range s.byStatus {
		if status >= lo && status <= hi {
			n += c
		}
	}
	return n
}

// percentile returns the p-th percentile of ds (nearest rank), rounded for
// printing. ds must not be empty.
func percentile(ds []time.Duration, p int) time.Duration {
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	i := (len(sorted)*p + 99) / 100 // the nearest rank, 1-based
	return sorted[max(i, 1)-1].Round(10 * time.Microsecond)
}

func pounds(pence int64) string { return fmt.Sprintf("£%d.%02d", pence/100, pence%100) }
