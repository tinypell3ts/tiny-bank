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

// Start is how a database service starts: it connects to url, retrying until
// the database answers, then applies migrations (see Migrate), all within
// StartupTimeout or ctx, whichever ends first. It returns the pool, which the
// caller closes on shutdown.
//
// Doing both before serving means a service never takes traffic with an old
// schema. A new pod can start before its brand-new database is up, so a few
// failed attempts are normal; each is logged (Open's errors never include
// the password).
func Start(ctx context.Context, logger *slog.Logger, url string, migrations fs.FS) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, StartupTimeout)
	defer cancel()

	pool, err := connect(ctx, logger, url)
	if err != nil {
		return nil, err
	}
	applied, err := Migrate(ctx, pool, migrations)
	if err != nil {
		pool.Close()
		return nil, err
	}
	logger.Info("database migrated", "applied", applied)
	return pool, nil
}

func connect(ctx context.Context, logger *slog.Logger, url string) (*pgxpool.Pool, error) {
	for {
		pool, err := Open(ctx, url)
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
