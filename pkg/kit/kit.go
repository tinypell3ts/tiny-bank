// Package kit is the shared foundation of every tiny-bank HTTP service:
// config from the environment, JSON logs, running the server with graceful
// shutdown, request IDs, access logs, JSON responses and the liveness check.
//
// A service wires these together with its own routes, readiness check and
// startup steps. Anything that differs between services stays in the service.
package kit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Main runs a service's run function until SIGTERM or Ctrl-C, then exits:
// 0 if run returned nil, 1 with the error on stderr otherwise.
//
// SIGTERM is what Kubernetes sends when it wants a pod to stop. It cancels
// the context, which tells Serve to shut down gracefully.
//
// run takes its dependencies (environment and log output) as arguments, so
// tests can call it directly with a fake environment.
func Main(name string, run func(ctx context.Context, getenv func(string) string, out io.Writer) error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Getenv, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

// NewLogger returns a JSON logger to out at level. Every line carries
// "service": name, so lines from different services can be told apart.
func NewLogger(out io.Writer, level slog.Level, name string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})).With("service", name)
}
