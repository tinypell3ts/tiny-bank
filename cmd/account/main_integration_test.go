//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// syncBuffer collects the service's log lines; run writes from its own goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// freePort asks the OS for an unused port, so the test server doesn't clash.
func freePort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// startService runs the real service against url and returns its base URL.
// It stops when the test ends.
func startService(t *testing.T, url string) (base string, logs *syncBuffer) {
	t.Helper()
	port := freePort(t)
	env := map[string]string{"DATABASE_URL": url, "PORT": port}
	logs = &syncBuffer{}

	// run's result is stored once and done is closed after it, so both the
	// readiness loop and cleanup can wait on done without taking the result
	// from each other (a channel send can only be received once).
	ctx, cancel := context.WithCancel(context.Background())
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = run(ctx, func(k string) string { return env[k] }, logs)
		close(done)
	}()
	t.Cleanup(func() {
		cancel() // what SIGTERM does in main
		<-done
		if runErr != nil {
			t.Errorf("run: %v\nlogs:\n%s", runErr, logs)
		}
	})

	base = "http://127.0.0.1:" + port
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := get(t, base+"/readyz"); status == http.StatusOK {
			return base, logs
		}
		select {
		case <-done:
			t.Fatalf("service exited early: %v\nlogs:\n%s", runErr, logs)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("service never became ready\nlogs:\n%s", logs)
	return "", nil
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestServiceMigratesThenServes(t *testing.T) {
	url := dbtest.StartPostgres(t, "s3cret")
	_, logs := startService(t, url)

	// The first start applied the accounts migration and said so.
	if !strings.Contains(logs.String(), `"applied":["00001_create_accounts.sql"]`) {
		t.Errorf("expected the migration to be logged, got:\n%s", logs)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Error("logs leak the database password")
	}

	// The accounts table exists with the columns we expect.
	pool, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var holder, currency, status string
	err = pool.QueryRow(t.Context(),
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES (gen_random_uuid(), 'Ada', 'GBP') RETURNING holder_name, currency, status",
	).Scan(&holder, &currency, &status)
	if err != nil {
		t.Fatalf("insert into accounts: %v", err)
	}
	if holder != "Ada" || currency != "GBP" || status != "open" {
		t.Errorf("got %s %s %s, want Ada GBP open", holder, currency, status)
	}

	// Every currency pkg/money supports is accepted.
	for _, c := range []string{"GBP", "EUR", "USD", "JPY"} {
		if _, err := money.ParseCurrency(c); err != nil {
			t.Fatalf("test list is stale: %v", err)
		}
		if _, err := pool.Exec(t.Context(), "INSERT INTO accounts (customer_id, holder_name, currency) VALUES (gen_random_uuid(), 'Ada', $1)", c); err != nil {
			t.Errorf("rejected supported currency %s: %v", c, err)
		}
	}

	// The table's own checks reject bad data, even if the code ever didn't.
	for _, bad := range []string{
		"INSERT INTO accounts (holder_name, currency) VALUES ('Ada', 'GBP')", // no customer
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES (gen_random_uuid(), '', 'GBP')",
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES (gen_random_uuid(), 'Ada', 'gbp')",
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES (gen_random_uuid(), 'Ada', 'GPB')", // a typo of a real code
		"INSERT INTO accounts (customer_id, holder_name, currency) VALUES (gen_random_uuid(), 'Ada', 'XYZ')", // well-formed but unsupported
		"INSERT INTO accounts (customer_id, holder_name, currency, status) VALUES (gen_random_uuid(), 'Ada', 'GBP', 'frozen')",
	} {
		if _, err := pool.Exec(t.Context(), bad); err == nil {
			t.Errorf("accepted %q, want a check constraint error", bad)
		}
	}
}

func TestServiceRestartAppliesNothing(t *testing.T) {
	url := dbtest.StartPostgres(t, "s3cret")
	startService(t, url) // first deploy
	_, logs := startService(t, url)

	if !strings.Contains(logs.String(), `"applied":[]`) {
		t.Errorf("second start should apply nothing, got:\n%s", logs)
	}
}

func TestReadyzFailsWhenTheDatabaseGoesAway(t *testing.T) {
	url := dbtest.StartPostgres(t, "s3cret")
	base, _ := startService(t, url)

	// Simulate the database going away by ending the service's own
	// connections from inside Postgres, then blocking new ones. The admin
	// connection uses the built-in "postgres" database, because Postgres won't
	// block connections to the database you're connected to.
	admin, err := db.Open(t.Context(), strings.Replace(url, "/app?", "/postgres?", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(t.Context(), "ALTER DATABASE app WITH ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(),
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'app' AND pid <> pg_backend_pid()"); err != nil {
		t.Fatal(err)
	}

	status, body := get(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d %s, want 503 while the database is unreachable", status, body)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil || got["status"] != "database unavailable" {
		t.Errorf("readyz body = %s, want status database unavailable", body)
	}
	if status, _ := get(t, base+"/healthz"); status != http.StatusOK {
		t.Errorf("healthz = %d, want 200: liveness must not depend on the database", status)
	}

	// Let connections back in: readiness recovers without a restart.
	if _, err := admin.Exec(t.Context(), "ALTER DATABASE app WITH ALLOW_CONNECTIONS true"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := get(t, base+"/readyz"); status == http.StatusOK {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("readyz never recovered after the database came back")
}

func TestAccountAPIEndToEnd(t *testing.T) {
	base, _ := startService(t, dbtest.StartPostgres(t, "s3cret"))

	// Open an account.
	body := `{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada Lovelace","currency":"GBP"}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/accounts", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var created struct {
		ID, Currency, Status string
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /accounts = %d, want 201", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Currency != "GBP" || created.Status != "open" {
		t.Fatalf("POST /accounts body = %+v, want a GBP open account with an ID", created)
	}
	location := resp.Header.Get("Location")

	// Follow Location back to the same account, now read from Postgres.
	status, got := get(t, base+location)
	if status != http.StatusOK || !strings.Contains(got, `"holder_name":"Ada Lovelace"`) || !strings.Contains(got, created.ID) {
		t.Fatalf("GET %s = %d %s, want the account just created", location, status, got)
	}

	if status, _ := get(t, base+"/accounts/not-a-uuid"); status != http.StatusNotFound {
		t.Errorf("GET /accounts/not-a-uuid = %d, want 404", status)
	}

	// Metrics label the route pattern, never the account ID (cardinality).
	_, metrics := get(t, base+"/metrics")
	if !strings.Contains(metrics, `http_route="/accounts/:id"`) || !strings.Contains(metrics, `http_route="/accounts"`) {
		t.Error("/metrics should count requests by route pattern")
	}
	if strings.Contains(metrics, created.ID) {
		t.Error("/metrics must not contain account IDs")
	}
}
