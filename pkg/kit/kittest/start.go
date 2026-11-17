package kittest

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// Logs collects a service's log lines. The service writes from its own
// goroutine, so it's safe to read while the service runs.
type Logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Start runs a service's real run function, the one its main passes to
// kit.Main, with env as its environment and a free PORT, and waits until
// GET /readyz answers 200. It returns the service's base URL and its logs.
//
// When the test ends, Start cancels run's context (what SIGTERM does in
// main), waits for it to return, and fails the test if it returned an error.
func Start(t *testing.T, run func(ctx context.Context, getenv func(string) string, out io.Writer) error,
	env map[string]string,
) (base string, logs *Logs) {
	t.Helper()
	port := freePort(t)
	env = maps.Clone(env)
	env["PORT"] = port
	logs = &Logs{}

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
		cancel()
		<-done
		if runErr != nil {
			t.Errorf("run: %v\nlogs:\n%s", runErr, logs)
		}
	})

	base = "http://127.0.0.1:" + port
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := Get(t, base+"/readyz"); status == http.StatusOK {
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

// Get fetches url and returns its status and body, or status 0 if the
// service didn't answer.
func Get(t *testing.T, url string) (int, string) {
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

// freePort asks the OS for an unused port, so test servers don't clash.
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

// Serve runs routes behind kit.Handler on a test server, ready, as a real
// service runs, for a test that needs another service to call (the ledger's
// tests run a real account service this way). name is the service name in
// its log lines, which come back in logs. The server stops when the test ends.
func Serve(t *testing.T, name string, routes func(router *typhon.Router)) (url string, logs *Logs) {
	t.Helper()
	router := typhon.Router{}
	routes(&router)
	logs = &Logs{}
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(kit.Handler(kit.NewLogger(logs, slog.LevelInfo, name), obs.NewRegistry(), &router, &ready, nil))
	t.Cleanup(srv.Close)
	return srv.URL, logs
}

// Post sends body to url as JSON, with header's values too if it isn't nil,
// and returns the answer's status, headers and body. It reads and closes the
// response itself.
func Post(t *testing.T, url, body string, header http.Header) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}
