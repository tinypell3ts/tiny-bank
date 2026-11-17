//go:build integration

package db_test

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
)

// TestRunStartsAndStopsBackgroundWork: background work starts once the
// database is ready, ends when the service shuts down, and Run returns only
// after it has, so it never loses its database mid-write.
func TestRunStartsAndStopsBackgroundWork(t *testing.T) {
	dsn := startPostgres(t)
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()

	var started, stopped atomic.Bool
	run := db.Run("test", goodMigrations, func(_ *typhon.Router, deps db.Deps) error {
		deps.Background(func(ctx context.Context) {
			if err := deps.Pool.Ping(ctx); err != nil {
				t.Errorf("background work started before the database was ready: %v", err)
			}
			started.Store(true)
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond) // finishing a write
			stopped.Store(true)
		})
		return nil
	})

	ctx, shutdown := context.WithCancel(t.Context())
	env := map[string]string{"PORT": port, "DATABASE_URL": dsn}
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) string { return env[k] }, io.Discard) }()

	for deadline := time.Now().Add(10 * time.Second); !started.Load(); time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("run stopped before its background work started: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("background work never started")
		}
	}
	shutdown()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run didn't return after shutdown")
	}
	if !stopped.Load() {
		t.Error("run returned before its background work finished")
	}
}
