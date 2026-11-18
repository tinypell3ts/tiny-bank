// Package outbox sends a service's events to Kafka if, and only if, the
// change they announce was saved: a transactional outbox.
//
// A service can't save a change to Postgres and send its event to Kafka
// atomically. Save first and crash, and the event is lost; send first and
// roll back, and the event announces something that didn't happen. So the
// event is saved instead, in an outbox table, in the same transaction as the
// change (Add), and sent to Kafka from there, in order, once it's
// committed.
//
// Delivery is at least once, so every event carries its own ID, and
// consumers skip one they've already handled.
//
// The service owns its outbox table, created by one of its migrations:
//
//	CREATE TABLE outbox (
//	    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
//	    topic       text NOT NULL,
//	    key         bytea NOT NULL,
//	    value       bytea NOT NULL,
//	    traceparent text NOT NULL,
//	    created_at  timestamptz NOT NULL DEFAULT now()
//	);
package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"
)

// Tx is what Add writes with: the caller's pgx.Tx.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Add saves event to be sent to topic, keyed by key, as binary protobuf. Call
// it with the transaction that saves the change the event is about, so the
// event is sent only if that commits. The trace in ctx is saved with it, so
// the trace carries on to whatever handles the event.
func Add(ctx context.Context, tx Tx, topic string, key []byte, event proto.Message) error {
	value, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("outbox: marshal event for %s: %w", topic, err)
	}
	trace := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, trace)
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (topic, key, value, traceparent) VALUES ($1, $2, $3, $4)`,
		topic, key, value, trace.Get("traceparent")); err != nil {
		return fmt.Errorf("outbox: add event for %s: %w", topic, err)
	}
	return nil
}
