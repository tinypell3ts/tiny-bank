package db

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// Deps is what a service's routes are built from.
type Deps struct {
	// Pool is the service's database, connected and migrated.
	Pool *pgxpool.Pool
	// Getenv reads settings of the service's own, such as where another
	// service is.
	Getenv func(string) string
	// Metrics is where the service registers its business metrics.
	// kit.Handler serves them at GET /metrics with the HTTP metrics.
	Metrics prometheus.Registerer
}

// Routes adds a service's API to router, built from deps. An error, like a
// missing setting, stops the service starting.
type Routes func(router *typhon.Router, deps Deps) error

// Run returns the whole of a service that has its own database, ready for
// kit.Main, which calls it with the real environment and stdout:
//
//	var run = db.Run("ledger", ledger.Migrations(), routes)
//
//	func main() { kit.Main("ledger", run) }
//
// It does, in order: load config (LoadConfig), start JSON logs, add the
// service's routes on a pool that hasn't connected yet, wait for the database
// and migrate it (Start), then listen and serve the routes behind kit.Handler
// until ctx ends, with /readyz checking the database. Tests run the same
// function with kittest.Start.
//
// Routes come before the database, so a bad setting of the service's own
// (a missing ACCOUNT_URL, say) stops it at once, before it waits up to
// StartupTimeout for a database or migrates one. Routes must only keep the
// pool, not query it. The port is opened last, so nothing is left listening
// when a step fails.
func Run(name string, migrations fs.FS, routes Routes) func(ctx context.Context, getenv func(string) string, out io.Writer) error {
	return func(ctx context.Context, getenv func(string) string, out io.Writer) error {
		cfg, err := LoadConfig(getenv)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		logger := kit.NewLogger(out, cfg.LogLevel, name)

		pool, err := newPool(ctx, cfg.DatabaseURL.Reveal()) // the one place the password is used
		if err != nil {
			return err
		}
		defer pool.Close() // after Serve returns, so in-flight requests can finish their queries

		router := typhon.Router{}
		reg := obs.NewRegistry()
		if err := routes(&router, Deps{Pool: pool, Getenv: getenv, Metrics: reg}); err != nil {
			return fmt.Errorf("routes: %w", err)
		}

		if err := Start(ctx, logger, pool, migrations); err != nil {
			return err
		}

		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		var ready atomic.Bool
		h := kit.Handler(logger, reg, &router, &ready, pool.Ping)
		return kit.Serve(ctx, ln, h, &ready, logger, cfg.ShutdownTimeout)
	}
}
