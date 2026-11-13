package kit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readyzHandler answers 200 while ready, 503 otherwise, like a service's /readyz.
func readyzHandler(ready *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestServeShutsDownIdleServer(t *testing.T) {
	ln := listen(t)
	url := "http://" + ln.Addr().String() + "/readyz"

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, readyzHandler(&ready), &ready, 5*time.Second)

	// While running, the server is ready.
	if status, err := doGet(t.Context(), url); err != nil || status != http.StatusOK {
		t.Fatalf("readyz = %d, %v; want %d", status, err, http.StatusOK)
	}

	// Cancelling the context (what SIGTERM does in main) must make serve return cleanly.
	cancel()
	if err := waitFor(t, serveErr); err != nil {
		t.Fatalf("serve returned error: %v", err)
	}

	// After shutdown the port is closed.
	if _, err := doGet(t.Context(), url); err == nil {
		t.Fatal("expected request to fail after shutdown")
	}
}

func TestServeWaitsForInFlightRequest(t *testing.T) {
	ln := listen(t)
	h, started, release := blockingHandler()

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, h, &ready, 5*time.Second)

	// Start a request that stays open until we release it.
	respStatus := make(chan int, 1)
	go func() {
		status, _ := doGet(context.WithoutCancel(t.Context()), "http://"+ln.Addr().String()+"/")
		respStatus <- status // 0 if the request failed
	}()
	<-started

	// Shut down while that request is still running.
	cancel()

	// serve must wait for the request instead of returning straight away.
	select {
	case err := <-serveErr:
		t.Fatalf("serve returned (%v) while a request was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	if ready.Load() {
		t.Error("ready should be false once shutdown has started")
	}

	// Let the request finish: the client gets its response and serve returns cleanly.
	close(release)
	if status := <-respStatus; status != http.StatusOK {
		t.Errorf("in-flight request status = %d, want %d", status, http.StatusOK)
	}
	if err := waitFor(t, serveErr); err != nil {
		t.Fatalf("serve returned error: %v", err)
	}
}

func TestServeGivesUpAfterShutdownTimeout(t *testing.T) {
	ln := listen(t)
	h, started, release := blockingHandler()
	t.Cleanup(func() { close(release) }) // unblock the handler when the test ends

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, h, &ready, 50*time.Millisecond)

	go func() { _, _ = doGet(context.WithoutCancel(t.Context()), "http://"+ln.Addr().String()+"/") }()
	<-started
	cancel()

	// The request never finishes, so shutdown must stop waiting and report it.
	err := waitFor(t, serveErr)
	if err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("err = %v, want a shutdown timeout error", err)
	}
}

func TestServeCancelsRequestsLeftAtTheShutdownDeadline(t *testing.T) {
	// A handler stuck on a dependency that never answers, like a query to an
	// unresponsive database: it only stops when its context is cancelled.
	ln := listen(t)
	started := make(chan struct{})
	stopped := make(chan error, 1)
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		stopped <- r.Context().Err()
	})

	var ready atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveErr := startServe(ctx, ln, h, &ready, 50*time.Millisecond)

	go func() { _, _ = doGet(context.WithoutCancel(t.Context()), "http://"+ln.Addr().String()+"/") }()
	<-started
	cancel()

	if err := waitFor(t, serveErr); err == nil || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("err = %v, want a shutdown timeout error", err)
	}
	// By the time Serve returns, the stuck request has been told to stop, so
	// closing the database pool next won't wait for it.
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("request context err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the stuck request was never cancelled after the shutdown deadline")
	}
}

// --- helpers ---

// listen opens a listener on a free local port (port 0 = let the OS pick).
func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// startServe runs Serve in a goroutine and returns a channel that receives its result.
func startServe(ctx context.Context, ln net.Listener, h http.Handler, ready *atomic.Bool, timeout time.Duration) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, ln, h, ready, slog.New(slog.DiscardHandler), timeout) }()
	return errCh
}

// waitFor returns Serve's result, failing the test if it takes too long.
func waitFor(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
		return nil
	}
}

// blockingHandler signals on started when a request arrives, then holds it open
// until release is closed.
func blockingHandler() (h http.Handler, started <-chan struct{}, release chan struct{}) {
	s := make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	h = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(s) })
		<-release
		w.WriteHeader(http.StatusOK)
	})
	return h, s, release
}

// doGet makes a GET request and returns the status code. It doesn't call t.Fatal,
// so it's safe to use from other goroutines.
func doGet(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
