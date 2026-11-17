package kit

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// tracerName is the instrumentation scope of the kit's spans.
const tracerName = "github.com/tinypell3ts/tiny-bank/pkg/kit"

// tracingFilter gives every request a server span, so one request can be
// followed across services. It continues the trace the caller sent in the
// W3C traceparent header, or starts a new one, and puts the span on the
// request's context, where the access log (and the calls this service
// makes) find it. Tracing itself is set up by obs.StartTracing; without an
// endpoint the span is a no-op.
//
// The span holds the same fixed attributes as the HTTP metrics, never a
// raw path or anything the caller typed: its name is the method and the
// route pattern ("POST /transfers"), and it records the method, route,
// status and, for an error, its terror code. A 5xx marks the span as
// failed; a 4xx is the caller's mistake, so it doesn't.
//
// Probes (/healthz, /readyz) get no span: Kubernetes calls them every few
// seconds, and they'd bury real traffic.
func tracingFilter(router *typhon.Router) typhon.Filter {
	return func(req typhon.Request, next typhon.Service) typhon.Response {
		if req.URL.Path == "/healthz" || req.URL.Path == "/readyz" {
			return next(req)
		}
		method := obs.MethodLabel(req.Method)
		ctx := otel.GetTextMapPropagator().Extract(req.Context, propagation.HeaderCarrier(req.Header))
		// Looked up per request, not once, so the tracer always comes from
		// the provider set now (a test may swap it after building a handler).
		ctx, span := otel.Tracer(tracerName).Start(ctx, method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		req.Context = ctx

		rsp := next(req)

		route := obs.RouteLabel(routeOf(req, rsp, router))
		status := statusOf(rsp)
		span.SetName(method + " " + route)
		span.SetAttributes(
			attribute.String("http.request.method", method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
		)
		var terr *terrors.Error
		if errors.As(rsp.Error, &terr) {
			// Codes are constants in each service's code, never built from
			// what a caller sent, so they're a small, fixed set.
			span.SetAttributes(attribute.String("error.type", terr.Code))
		}
		if status >= 500 {
			span.SetStatus(codes.Error, "")
		}
		return rsp
	}
}

// traceIDFrom is the ID of the trace ctx's span belongs to, for log lines,
// or "" when there's no trace (tracing off, or a probe).
func traceIDFrom(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// routeOf is the route pattern the router matched for req ("/accounts/:id"),
// or "" if none did. The router records it on the request it hands the
// handler, and the response carries that request back out. A response built
// from the unrouted request, like the 500 from recoverFilter after a panic,
// doesn't carry it, so only then does it ask the router to match the path
// again.
func routeOf(req typhon.Request, rsp typhon.Response, router *typhon.Router) string {
	if rsp.Request != nil {
		if route, ok := typhon.RequestPatternFromContext(rsp.Request.Context); ok && route != "" {
			return route
		}
	}
	return router.Pattern(req)
}

// forwardTrace sets the W3C traceparent header on an outgoing request from
// the span in its context, so the other service continues this trace.
func forwardTrace(req typhon.Request, next typhon.Service) typhon.Response {
	otel.GetTextMapPropagator().Inject(req.Context, propagation.HeaderCarrier(req.Header))
	return next(req)
}

// startCallSpan starts a client span for a call to target, under the span in
// ctx (the request being handled), so the trace shows where time went. It's
// named by method and the other service, from target's host name ("POST
// ledger"), never its path, which may hold IDs (the same rule as server
// spans), nor its port, so one service is one name. The port, if the URL has
// one, is recorded as server.port.
func startCallSpan(ctx context.Context, method, target string) (context.Context, trace.Span) {
	host := "unknown"
	attrs := []attribute.KeyValue{}
	if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
		host = u.Hostname()
		if port, err := strconv.Atoi(u.Port()); err == nil {
			attrs = append(attrs, attribute.Int("server.port", port))
		}
	}
	method = obs.MethodLabel(method)
	attrs = append(attrs, attribute.String("http.request.method", method), attribute.String("server.address", host))
	return otel.Tracer(tracerName).Start(ctx, method+" "+host, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
}

// endCallSpan ends a client span with the call's status, 0 if no response
// came back (wireStatus sets it only for a real one), and its error. Any
// failed call marks the span failed: from the caller's side a refusal is a
// failure too.
//
// error.type is the other service's terror code when it answered. When
// nothing came back (no connection, a timeout, no time left to try), it's
// "_OTHER", even though Typhon's ErrorFilter has by then turned the failure
// into an internal_service terror: recording that would make an unreachable
// service look like one that answered 500.
func endCallSpan(span trace.Span, status int, err error) {
	if status != 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", status))
	}
	if err != nil {
		errorType := "_OTHER"
		var terr *terrors.Error
		if status != 0 && errors.As(err, &terr) {
			errorType = terr.Code
		}
		span.SetAttributes(attribute.String("error.type", errorType))
		span.SetStatus(codes.Error, "")
	}
	span.End()
}
