package db

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope of the database's spans.
const tracerName = "github.com/tinypell3ts/tiny-bank/pkg/db"

// queryTracer gives each query, and each batch, a span under the request it
// runs for, so a trace shows where a request's time went in the database:
// waiting for a row lock shows as a long BEGIN or SELECT … FOR UPDATE.
//
// A span records the SQL text, which is always a fixed statement with $1
// placeholders (AGENTS.md: caller input never becomes SQL), and never the
// values passed for them, which hold amounts and names.
//
// A query with no span in its context gets none: the finisher's polling,
// readiness checks and migrations run outside any request, and would bury
// real traffic, as probes would.
type queryTracer struct{}

// spanKey holds, in a query's context, the span queryTracer started for it,
// so the end of the query ends that span and never the request's own.
type spanKey struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return startQuerySpan(ctx, operationOf(data.SQL), attribute.String("db.query.text", data.SQL))
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	endQuerySpan(ctx, data.Err)
}

func (queryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	return startQuerySpan(ctx, "BATCH", attribute.Int("db.operation.batch.size", data.Batch.Len()))
}

// TraceBatchQuery records each query in a batch as an event on the batch's
// span, with its SQL text: they share one round trip, so they share one span.
func (queryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if span, ok := ctx.Value(spanKey{}).(trace.Span); ok {
		span.AddEvent("query", trace.WithAttributes(attribute.String("db.query.text", data.SQL)))
	}
}

func (queryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	endQuerySpan(ctx, data.Err)
}

// startQuerySpan starts a client span called name under the span in ctx, if
// there is one, and keeps it under spanKey for endQuerySpan.
func startQuerySpan(ctx context.Context, name string, attr attribute.KeyValue) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	ctx, span := otel.Tracer(tracerName).Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "postgresql"), attr))
	return context.WithValue(ctx, spanKey{}, span)
}

// endQuerySpan ends the span startQuerySpan started, if it did, marking it
// failed on an error: error.type is Postgres's SQLSTATE code (23505 for a
// unique violation, P0001 for one of our triggers), or "_OTHER".
func endQuerySpan(ctx context.Context, err error) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	if err != nil {
		errorType := "_OTHER"
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			errorType = pgErr.Code
		}
		span.SetAttributes(attribute.String("error.type", errorType))
		span.SetStatus(codes.Error, "")
	}
	span.End()
}

// operationOf is a query's first keyword, upper-cased (SELECT, INSERT, BEGIN),
// as its span's name: short, fixed, and never a value.
func operationOf(sql string) string {
	words := strings.Fields(sql)
	if len(words) == 0 {
		return "QUERY"
	}
	return strings.ToUpper(strings.TrimRight(words[0], ";"))
}
