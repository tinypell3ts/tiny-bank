package db

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// tracer starts the database's spans.
var tracer = obs.NewTracer("github.com/tinypell3ts/tiny-bank/pkg/db")

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
	return startQuerySpan(ctx, summaryOf(data.SQL), attribute.String("db.query.text", data.SQL))
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	endQuerySpan(ctx, data.Err)
}

// TraceBatchStart starts one span for a batch, named by what's in it
// ("BATCH INSERT accounts, SELECT accounts FOR UPDATE"), as its queries share
// one round trip.
func (queryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	summaries := make([]string, len(data.Batch.QueuedQueries))
	for i, q := range data.Batch.QueuedQueries {
		summaries[i] = summaryOf(q.SQL)
	}
	return startQuerySpan(ctx, "BATCH "+strings.Join(summaries, ", "), attribute.Int("db.operation.batch.size", data.Batch.Len()))
}

// TraceBatchQuery records each query in a batch as an event on the batch's
// span, with its SQL text and, if it failed, its error.type, so a failed
// batch shows which of its queries failed.
func (queryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	attrs := []attribute.KeyValue{attribute.String("db.query.text", data.SQL)}
	if data.Err != nil {
		attrs = append(attrs, attribute.String("error.type", errorTypeOf(data.Err)))
	}
	span.AddEvent("query", trace.WithAttributes(attrs...))
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
	ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient),
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
		obs.FailSpan(span, errorTypeOf(err))
	}
	span.End()
}

// errorTypeOf is a query error's SQLSTATE code, or "_OTHER".
func errorTypeOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return "_OTHER"
}

// identifier is a plain SQL name: a table, or a function like
// pg_advisory_xact_lock.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_.]*$`)

// summaryOf names a query for its span: its operation and what it works on,
// "UPDATE transfers", "SELECT pg_advisory_xact_lock", "SELECT accounts FOR
// UPDATE", "BEGIN". The target is the table after FROM or INTO (or UPDATE's
// own), or for a SELECT with no FROM, the function it calls; it's kept only
// if it's a plain name, so a span name is never a value. Our SQL is fixed
// text (AGENTS.md), so the names are a small, fixed set.
func summaryOf(sql string) string {
	words := strings.Fields(sql)
	if len(words) == 0 {
		return "QUERY"
	}
	op := strings.ToUpper(strings.TrimRight(words[0], ";"))
	target := ""
	switch op {
	case "UPDATE":
		if len(words) > 1 {
			target = words[1]
		}
	case "INSERT":
		target = wordAfter(words, "INTO")
	case "SELECT", "DELETE":
		target = wordAfter(words, "FROM")
		if target == "" && op == "SELECT" && len(words) > 1 {
			target, _, _ = strings.Cut(words[1], "(") // a function: SELECT pg_advisory_xact_lock(…)
		}
	}
	name := op
	if target = strings.TrimRight(target, ",;"); identifier.MatchString(target) {
		name += " " + target
	}
	if strings.Contains(strings.ToUpper(sql), "FOR UPDATE") {
		name += " FOR UPDATE"
	}
	return name
}

// wordAfter is the word following keyword in words (any case), or "".
func wordAfter(words []string, keyword string) string {
	for i, w := range words[:len(words)-1] {
		if strings.EqualFold(w, keyword) {
			return words[i+1]
		}
	}
	return ""
}
