//go:build integration

package outbox

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

//go:embed testdata/migrations/*.sql
var testMigrations embed.FS

// migrated returns a database with an outbox table.
func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	migrations, err := fs.Sub(testMigrations, "testdata")
	if err != nil {
		t.Fatal(err)
	}
	return dbtest.Migrated(t, migrations)
}

// add saves each value as an event on topic, keyed "k", one transaction each.
func add(t *testing.T, ctx context.Context, pool *pgxpool.Pool, topic string, values ...string) {
	t.Helper()
	for _, v := range values {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return Add(ctx, tx, topic, []byte("k"), wrapperspb.String(v))
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func waiting(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAddSavesTheEventWithItsTrace, as binary protobuf, so it can be sent
// as it is and continue the trace of the request that added it.
func TestAddSavesTheEventWithItsTrace(t *testing.T) {
	pool := migrated(t)
	kittest.RecordSpans(t)
	ctx, request := otel.Tracer("test").Start(t.Context(), "POST /transfers")
	add(t, ctx, pool, "events", "one")
	request.End()

	var topic, traceparent string
	var key, value []byte
	if err := pool.QueryRow(t.Context(), `SELECT topic, key, value, traceparent FROM outbox`).Scan(&topic, &key, &value, &traceparent); err != nil {
		t.Fatal(err)
	}
	var event wrapperspb.StringValue
	if err := proto.Unmarshal(value, &event); err != nil {
		t.Fatal(err)
	}
	if topic != "events" || string(key) != "k" || event.GetValue() != "one" {
		t.Errorf("saved %s %s %q, want events k \"one\"", topic, key, event.GetValue())
	}
	if !strings.Contains(traceparent, request.SpanContext().TraceID().String()) {
		t.Errorf("traceparent = %q, want the request's trace", traceparent)
	}
}

// TestAnEventRolledBackIsNeverSaved: the event goes only if the change does.
func TestAnEventRolledBackIsNeverSaved(t *testing.T) {
	pool := migrated(t)
	rollBack := errors.New("the change failed")
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if err := Add(t.Context(), tx, "events", []byte("k"), wrapperspb.String("never")); err != nil {
			return err
		}
		return rollBack
	})
	if !errors.Is(err, rollBack) {
		t.Fatal(err)
	}
	if n := waiting(t, pool); n != 0 {
		t.Errorf("%d events in the outbox, want 0", n)
	}
}
