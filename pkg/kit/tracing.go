package kit

import (
	"context"
	"errors"

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
