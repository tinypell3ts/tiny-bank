package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
)

// Config is the shared kit.Config (PORT, LOG_LEVEL, SHUTDOWN_TIMEOUT) plus
// DATABASE_URL, for a service with its own database.
type Config struct {
	kit.Config
	// DatabaseURL is set by the Helm chart from CloudNativePG's secret. It
	// contains the password, so it's a kit.Secret and never prints.
	DatabaseURL kit.Secret
}

// LoadConfig reads a database service's config. DATABASE_URL is required.
// It takes getenv instead of calling os.Getenv so tests can pass a fake
// environment.
func LoadConfig(getenv func(string) string) (Config, error) {
	base, err := kit.LoadConfig(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Config: base, DatabaseURL: kit.Secret(getenv("DATABASE_URL"))}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}

const (
	// StartupTimeout bounds connecting and migrating together. On a brand-new
	// database CloudNativePG takes about a minute to be ready. The chart's
	// startup probe gives the pod longer than this before liveness checks begin.
	StartupTimeout = 2 * time.Minute
	// retryEvery is how long to wait between connection attempts at startup.
	retryEvery = 2 * time.Second
)

// Start is how a database service gets its database ready: it waits until
// pool's database answers, retrying, then applies migrations (see Migrate),
// all within StartupTimeout or ctx, whichever ends first.
//
// Doing both before serving means a service never takes traffic with an old
// schema. A new pod can start before its brand-new database is up, so a few
// failed attempts are normal; each is logged (pgx's errors never include the
// password).
func Start(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, migrations fs.FS) error {
	ctx, cancel := context.WithTimeout(ctx, StartupTimeout)
	defer cancel()

	if err := waitFor(ctx, logger, pool); err != nil {
		return err
	}
	applied, err := Migrate(ctx, pool, migrations)
	if err != nil {
		return err
	}
	logger.Info("database migrated", "applied", applied)
	return nil
}

func waitFor(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool) error {
	for {
		err := pool.Ping(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("database not reachable before startup timeout: %w", err)
		}
		logger.Warn("database not ready, retrying", "error", err.Error(), "retry_in", retryEvery.String())
		select {
		case <-ctx.Done():
			return fmt.Errorf("database not reachable before startup timeout: %w", errors.Join(err, ctx.Err()))
		case <-time.After(retryEvery):
		}
	}
}
