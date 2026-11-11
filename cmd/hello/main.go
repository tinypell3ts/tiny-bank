// Command hello is the smallest possible tiny-bank service. It exists to prove the
// foundations every real service will share: env config, JSON logs, request IDs,
// health and readiness endpoints, and graceful shutdown.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// SIGTERM is what Kubernetes sends when it wants a pod to stop; Ctrl-C sends SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hello:", err)
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
		With("service", "hello")

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	return serve(ctx, ln, logger, cfg.ShutdownTimeout)
}
