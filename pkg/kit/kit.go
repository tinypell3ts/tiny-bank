// Package kit is the shared foundation of every tiny-bank service: config
// from the environment (with Secret for anything secret), JSON logs, running
// the server with graceful shutdown, and Handler, which puts a service's
// Typhon router behind the filters every service shares: request IDs, access
// logs, metrics, safe errors and panic recovery. Decode reads a request
// body into a protobuf message strictly, and Respond writes one back as JSON.
// Handler also serves the probes every service needs: /healthz and /readyz.
//
// A service wires these together with its own routes and startup steps. Anything that differs between services stays in the service.
// Package kittest checks that a service is wired up correctly.
package kit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// Main runs a service's run function until SIGTERM or Ctrl-C, then exits:
// 0 if run returned nil, 1 with the error on stderr otherwise.
//
// SIGTERM is what Kubernetes sends when it wants a pod to stop. It cancels
// the context, which tells Serve to shut down gracefully.
//
// It first gives Go's garbage collector a memory limit just under the
// container's (see memoryLimitFor), so the service slows down as it nears
// its limit instead of being killed.
//
// It starts tracing (obs.StartTracing: on when OTEL_EXPORTER_OTLP_ENDPOINT is
// set) before run, and once run returns, sends the spans still buffered,
// waiting at most tracingFlushTimeout. A failed flush loses the last few
// spans, never the exit code.
//
// run takes its dependencies (environment and log output) as arguments, so
// tests can call it directly with a fake environment.
func Main(name string, run func(ctx context.Context, getenv func(string) string, out io.Writer) error) {
	if limit, ok := memoryLimitFor(os.Getenv); ok {
		debug.SetMemoryLimit(limit)
	}
	flushTracing, err := obs.StartTracing(name, os.Getenv, NewLogger(os.Stdout, slog.LevelInfo, name))
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, os.Getenv, os.Stdout)
	stop()
	flushCtx, cancel := context.WithTimeout(context.Background(), tracingFlushTimeout)
	if flushErr := flushTracing(flushCtx); flushErr != nil {
		fmt.Fprintln(os.Stderr, name+": flush traces:", flushErr)
	}
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

// tracingFlushTimeout bounds sending the last spans at shutdown. It fits in
// the time Kubernetes leaves after the server's own shutdown (the chart's
// terminationGracePeriodSeconds keeps 5s spare).
const tracingFlushTimeout = 3 * time.Second

// NewLogger returns a JSON logger to out at level. Every line carries
// "service": name, so lines from different services can be told apart, and
// every line logged with a context inside a trace (logger.ErrorContext(ctx,
// …), as the kit's own request logging is) carries its "trace_id", so a
// trace ID finds all of a request's lines, the full error included.
func NewLogger(out io.Writer, level slog.Level, name string) *slog.Logger {
	return slog.New(traceIDHandler{slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})}).With("service", name)
}

// traceIDHandler adds "trace_id" (TraceIDFrom) to each line logged with a
// context that has one, before handing it to the handler it wraps.
type traceIDHandler struct{ slog.Handler }

func (h traceIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := TraceIDFrom(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup keep the wrapper, so a logger made with With still
// adds the trace ID.
func (h traceIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceIDHandler) WithGroup(name string) slog.Handler {
	return traceIDHandler{h.Handler.WithGroup(name)}
}
