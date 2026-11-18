//go:build integration

package db_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// inTrace is the finished spans in request's trace, oldest first. Other
// spans are left out: testcontainers' Docker client makes spans of its own
// while it starts Postgres.
func inTrace(exporter *tracetest.InMemoryExporter, request oteltrace.Span) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.SpanContext.TraceID() == request.SpanContext().TraceID() {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// TestAQueryInARequestGetsASpan: under the request's span, with its SQL text
// but never the values passed for its placeholders.
func TestAQueryInARequestGetsASpan(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	pool, err := db.Open(t.Context(), dbtest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ctx, request := otel.Tracer("test").Start(t.Context(), "POST /journals")
	var n int
	if err := pool.QueryRow(ctx, "SELECT $1::int", 4242).Scan(&n); err != nil {
		t.Fatal(err)
	}
	request.End()

	spans := inTrace(exporter, request)
	if len(spans) != 2 { // the query's, then the request's
		t.Fatalf("got %d spans, want the query's and the request's", len(spans))
	}
	q := spans[0]
	if q.Name != "SELECT" || q.Parent.SpanID() != request.SpanContext().SpanID() ||
		attr(q, "db.system.name") != "postgresql" || attr(q, "db.query.text") != "SELECT $1::int" {
		t.Errorf("query span = %q under %s, %v; want SELECT under the request, with its SQL text", q.Name, q.Parent.SpanID(), q.Attributes)
	}
	for _, kv := range q.Attributes {
		if strings.Contains(kv.Value.String(), "4242") {
			t.Errorf("the span records a value passed to the query (%s): values hold amounts and names", kv.Key)
		}
	}
}

func TestAFailedQueryRecordsItsSQLSTATE(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	pool, err := db.Open(t.Context(), dbtest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx, request := otel.Tracer("test").Start(t.Context(), "request")
	_, _ = pool.Exec(ctx, "SELECT * FROM no_such_table")
	request.End()

	q := inTrace(exporter, request)[0]
	if q.Status.Code != codes.Error || attr(q, "error.type") != "42P01" { // undefined_table
		t.Errorf("failed query span = %v, %q; want failed with SQLSTATE 42P01", q.Status, attr(q, "error.type"))
	}
}

func TestABatchIsOneSpan(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	pool, err := db.Open(t.Context(), dbtest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx, request := otel.Tracer("test").Start(t.Context(), "request")
	batch := &pgx.Batch{}
	batch.Queue("SELECT 1")
	batch.Queue("SELECT 2")
	if err := pool.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatal(err)
	}
	request.End()

	b := inTrace(exporter, request)[0]
	if b.Name != "BATCH" || attr(b, "db.operation.batch.size") != "2" || len(b.Events) != 2 {
		t.Errorf("batch span = %q %v with %d events; want BATCH of 2, one event per query", b.Name, b.Attributes, len(b.Events))
	}
}

// TestAQueryOutsideARequestGetsNoSpan: polling, probes and migrations run
// outside any request, and mustn't bury real traffic.
func TestAQueryOutsideARequestGetsNoSpan(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	pool, err := db.Open(t.Context(), dbtest.Database(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	for _, s := range exporter.GetSpans() {
		if s.InstrumentationScope.Name == "github.com/tinypell3ts/tiny-bank/pkg/db" {
			t.Errorf("a query with no request made span %q, want none", s.Name)
		}
	}
}
