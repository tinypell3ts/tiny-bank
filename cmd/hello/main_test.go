package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsBadConfig(t *testing.T) {
	getenv := func(k string) string {
		if k == "PORT" {
			return "not-a-port"
		}
		return ""
	}

	err := run(t.Context(), getenv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("err = %v, want a load config error", err)
	}
}

// TestRunServesUntilCancelled runs the real service: it becomes ready, then
// stops cleanly when its context is cancelled (what SIGTERM does in main).
func TestRunServesUntilCancelled(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close() // free the port for run to use

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func(k string) string {
			if k == "PORT" {
				return port
			}
			return ""
		}, io.Discard)
	}()

	url := "http://127.0.0.1:" + port + "/readyz"
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("hello never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}
