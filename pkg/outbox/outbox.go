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
// The service owns its outbox table, created by one of its migrations: a copy
// of testdata/migrations/00001_create_outbox.sql, the one definition of it.
// Columns are read by name, so a copy that has drifted fails loudly on the
// first read rather than mixing columns up.
package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"
)

// Add saves event to be sent to topic, keyed by key, as binary protobuf. Call
// it with the transaction that saves the change the event is about, so the
// event is sent only if that commits. The trace in ctx (traceparent and
// tracestate) is saved with it, so the trace carries on to whatever handles
// the event.
func Add(ctx context.Context, tx pgx.Tx, topic string, key []byte, event proto.Message) error {
	value, err := proto.Marshal(event)
	if err != nil {
		return fmt.Errorf("outbox: marshal event for %s: %w", topic, err)
	}
	trace := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, trace)
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (topic, key, value, traceparent, tracestate) VALUES ($1, $2, $3, $4, $5)`,
		topic, key, value, trace.Get("traceparent"), trace.Get("tracestate")); err != nil {
		return fmt.Errorf("outbox: add event for %s: %w", topic, err)
	}
	return nil
}
