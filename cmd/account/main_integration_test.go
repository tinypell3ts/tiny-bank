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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, logs) }()
	t.Cleanup(func() {
		cancel() // what SIGTERM does in main
		if err := <-done; err != nil {
			t.Errorf("run: %v\nlogs:\n%s", err, logs)
		}
	})

	base = "http://127.0.0.1:" + port
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := get(t, base+"/readyz"); status == http.StatusOK {
			return base, logs
		}
		select {
		case err := <-done:
			t.Fatalf("service exited early: %v\nlogs:\n%s", err, logs)
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
		"INSERT INTO accounts (holder_name, currency) VALUES ('Ada', 'GBP') RETURNING holder_name, currency, status",
	).Scan(&holder, &currency, &status)
	if err != nil {
		t.Fatalf("insert into accounts: %v", err)
	}
	if holder != "Ada" || currency != "GBP" || status != "open" {
		t.Errorf("got %s %s %s, want Ada GBP open", holder, currency, status)
	}

	// The table's own checks reject bad data, even if the code ever didn't.
	for _, bad := range []string{
		"INSERT INTO accounts (holder_name, currency) VALUES ('', 'GBP')",
		"INSERT INTO accounts (holder_name, currency) VALUES ('Ada', 'gbp')",
		"INSERT INTO accounts (holder_name, currency, status) VALUES ('Ada', 'GBP', 'frozen')",
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
