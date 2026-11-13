// Command hello is the smallest possible tiny-bank service. It exists to prove the
// foundations every real service shares, from pkg/kit: env config, JSON logs,
// request IDs, health and readiness endpoints, metrics and graceful shutdown.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

func main() { kit.Main("hello", run) }

// run holds everything main does, with its dependencies passed in so tests can call it.
func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	cfg, err := kit.LoadConfig(getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := kit.NewLogger(out, cfg.LogLevel, "hello")

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	var ready atomic.Bool
	return kit.Serve(ctx, ln, newHandler(logger, &ready, obs.NewRegistry()), &ready, logger, cfg.ShutdownTimeout)
}
