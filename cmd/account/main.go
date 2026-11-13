// Command account is the account service: it opens and looks up customers'
// accounts, stored in its own Postgres database.
//
// It shares its foundations with cmd/hello (env config, JSON logs, request IDs,
// health, metrics, graceful shutdown). That code is copied for now and moves to
// pkg/kit in Phase 5.5, once there are two services to share it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
	"github.com/tinypell3ts/tiny-bank/services/account"
)

const (
	// startupTimeout bounds connecting and migrating together. On a brand-new
	// database CloudNativePG takes about a minute to be ready. The chart's
	// startup probe gives the pod longer than this before liveness checks begin.
	startupTimeout = 2 * time.Minute
	// retryEvery is how long to wait between connection attempts at startup.
	retryEvery = 2 * time.Second
)

func main() {
	// SIGTERM is what Kubernetes sends when it wants a pod to stop; Ctrl-C sends SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "account:", err)
		os.Exit(1)
	}
}

// run holds everything main does, with its dependencies passed in so tests can call it.
func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", "account")

	// Connect and migrate before serving anything, so the service never takes
	// traffic with an old schema.
	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	pool, err := connect(startCtx, logger, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close() // after serve returns, so in-flight requests can finish their queries

	applied, err := db.Migrate(startCtx, pool, account.Migrations())
	if err != nil {
		return err
	}
	logger.Info("database migrated", "applied", applied)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	var ready atomic.Bool
	return serve(ctx, ln, newHandler(logger, &ready, obs.NewRegistry(), pool), &ready, logger, cfg.ShutdownTimeout)
}

// connect opens the database, retrying until it answers or ctx ends. A new
// pod can start before its brand-new database is up, so a few failed
// attempts are normal; each is logged (db.Open's errors never include the password).
func connect(ctx context.Context, logger *slog.Logger, url string) (*pgxpool.Pool, error) {
	for {
		pool, err := db.Open(ctx, url)
		if err == nil {
			return pool, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("database not reachable before startup timeout: %w", err)
		}
		logger.Warn("database not ready, retrying", "error", err.Error(), "retry_in", retryEvery.String())
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("database not reachable before startup timeout: %w", errors.Join(err, ctx.Err()))
		case <-time.After(retryEvery):
		}
	}
}
