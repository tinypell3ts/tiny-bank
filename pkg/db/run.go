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

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// Routes adds a service's API to router, built on its database pool. getenv
// reads any settings of the service's own (such as where another service
// is); an error, like a missing setting, stops the service starting.
type Routes func(router *typhon.Router, pool *pgxpool.Pool, getenv func(string) string) error

// Run returns the whole of a service that has its own database, ready for
// kit.Main, which calls it with the real environment and stdout:
//
//	var run = db.Run("ledger", ledger.Migrations(), routes)
//
//	func main() { kit.Main("ledger", run) }
//
// It does, in order: load config (LoadConfig), start JSON logs, connect and
// migrate (Start), add the service's routes, then serve them behind
// kit.Handler until ctx ends, with /readyz checking the database. Tests run
// the same function with kittest.Start.
func Run(name string, migrations fs.FS, routes Routes) func(ctx context.Context, getenv func(string) string, out io.Writer) error {
	return func(ctx context.Context, getenv func(string) string, out io.Writer) error {
		cfg, err := LoadConfig(getenv)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		logger := kit.NewLogger(out, cfg.LogLevel, name)

		pool, err := Start(ctx, logger, cfg.DatabaseURL.Reveal(), migrations) // the one place the password is used
		if err != nil {
			return err
		}
		defer pool.Close() // after Serve returns, so in-flight requests can finish their queries

		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}

		router := typhon.Router{}
		if err := routes(&router, pool, getenv); err != nil {
			return fmt.Errorf("routes: %w", err)
		}
		var ready atomic.Bool
		h := kit.Handler(logger, obs.NewRegistry(), &router, &ready, pool.Ping)
		return kit.Serve(ctx, ln, h, &ready, logger, cfg.ShutdownTimeout)
	}
}
