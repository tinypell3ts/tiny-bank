package obs

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracingEndpoint is the setting that turns tracing on: the base URL of an
// OTLP/HTTP receiver, such as http://jaeger.jaeger:4318 on the local cluster.
// Spans go to its /v1/traces. It's the OpenTelemetry standard name, so the
// same setting works for any receiver.
const TracingEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

// StartTracing sets up tracing for the service called name, from getenv,
// and returns a function that sends any spans still buffered (call it on
// shutdown, with a deadline).
//
// With TracingEndpoint set, spans are batched in memory and sent over
// OTLP/HTTP in the background; a request never waits for an export. Every
// span is kept (parent-based, always on), so a trace is never missing a hop.
// Trace context travels between services as the W3C traceparent header.
//
// Without it, tracing is off: spans cost almost nothing and go nowhere. So
// tests and a plain `go run` need no receiver.
//
// It sets OpenTelemetry's global tracer provider and propagator, which is
// how the rest of the kit finds them. Export failures (the receiver is down,
// say) are logged to logger as warnings and never fail a request.
func StartTracing(name string, getenv func(string) string, logger *slog.Logger) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("tracing", "error", err)
	}))

	endpoint := getenv(TracingEndpoint)
	if endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	tracesURL, err := tracesURLFrom(endpoint)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(tracesURL))
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", name))),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// tracesURLFrom is the URL spans are sent to: v1/traces added to endpoint's
// path, as the OTLP spec says for a base URL, keeping any query string after
// it (http://collector/otel?key=k becomes http://collector/otel/v1/traces?key=k).
// endpoint must be an absolute http or https URL, so a typo stops the service
// at startup instead of losing every trace.
func tracesURLFrom(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("tracing: %s must be an http or https URL, like http://jaeger.jaeger:4318", TracingEndpoint)
	}
	return u.JoinPath("v1", "traces").String(), nil
}
