// Package outbox sends a service's events to Kafka if, and only if, the
// change they announce was saved: a transactional outbox.
//
// A service can't save a change to Postgres and send its event to Kafka
// atomically. Save first and crash, and the event is lost; send first and
// roll back, and the event announces something that didn't happen. So the
// event is saved instead, in an outbox table, in the same transaction as the
// change (Add). A Relay, running beside the service, sends what's in the
// table to Kafka, and deletes each event once Kafka has stored it.
//
// Events with the same topic and key reach Kafka in the order their rows
// were inserted: one that Kafka won't take holds back the rest of its key,
// and only its key. Insert order is commit order only for transactions that
// don't overlap, so never add events for one key from two transactions at
// once. Events with different keys have no order between them.
//
// Delivery is at least once: a relay that stops after sending but before
// deleting sends that event again. So every event carries its own ID, and
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
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/protobuf/proto"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/kafka"
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

const (
	// batchSize is how many events a relay sends per transaction.
	batchSize = 100
	// pollInterval is how long a relay waits when there was nothing to send,
	// another pod was relaying, or sending failed.
	pollInterval = 200 * time.Millisecond
	// sendTimeout bounds a batch's sends, so a relay whose Kafka is down
	// gives up and rolls back rather than holding its transaction open.
	sendTimeout = 10 * time.Second
	// measureInterval is how often a relay sets its backlog metrics.
	measureInterval = time.Second
	// relayLock is the advisory lock a relay holds while it sends a batch,
	// so only one pod sends at a time and events keep their order. Every
	// service has its own database, so one key is enough.
	relayLock = 7_210_000_001
)

// Relay sends what's in the outbox to Kafka.
type Relay struct {
	pool     *pgxpool.Pool
	producer *kafka.Producer
	logger   *slog.Logger
	metrics  *metrics
}

// NewRelay returns a Relay for the outbox in pool, sending with producer.
// Its metrics are registered on reg.
func NewRelay(pool *pgxpool.Pool, producer *kafka.Producer, logger *slog.Logger, reg prometheus.Registerer) *Relay {
	return &Relay{pool: pool, producer: producer, logger: logger, metrics: newMetrics(reg)}
}

// Run sends events until ctx ends. Run it on every pod: they take turns, one
// batch at a time. It measures the backlog every measureInterval meanwhile,
// even while a batch is stuck.
func (r *Relay) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { r.measureEvery(ctx) })

	for ctx.Err() == nil {
		sent, err := r.SendBatch(ctx)
		if err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "outbox relay", "error", err)
		}
		if sent > 0 {
			continue // there may be more waiting
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}

// row is an event waiting in the outbox.
type row struct {
	ID          int64  `db:"id"`
	Topic       string `db:"topic"`
	Key         []byte `db:"key"`
	Value       []byte `db:"value"`
	Traceparent string `db:"traceparent"`
	Tracestate  string `db:"tracestate"`
}

// SendBatch sends the oldest waiting event of each key among the oldest
// batchSize events, all together, and deletes those Kafka stored, in one
// transaction holding the relay lock, within sendTimeout. A key's next event
// waits for a later batch, once this one is stored, so a key's events never
// overtake each other however Kafka answers. It returns how many it sent: 0
// if another pod holds the lock.
func (r *Relay) SendBatch(ctx context.Context) (int, error) {
	var sent []int64
	var sendErr error
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, relayLock).Scan(&locked); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if !locked {
			return nil // another pod is relaying
		}
		rows, err := tx.Query(ctx, `SELECT `+db.Columns[row]()+` FROM outbox ORDER BY id LIMIT $1`, batchSize)
		if err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		waiting, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
		if err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		var ids []int64
		var outgoing []kafka.Outgoing
		keys := map[string]bool{}
		for _, e := range waiting {
			key := e.Topic + "\x00" + string(e.Key)
			if keys[key] {
				continue // its key's earlier event goes first
			}
			keys[key] = true
			trace := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": e.Traceparent, "tracestate": e.Tracestate})
			ids = append(ids, e.ID)
			outgoing = append(outgoing, kafka.Outgoing{Topic: e.Topic, Key: e.Key, Value: e.Value, Trace: trace})
		}
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		errs := r.producer.ProduceAll(sendCtx, outgoing)
		cancel()
		for i, err := range errs {
			if err == nil {
				sent = append(sent, ids[i])
			} else if sendErr == nil {
				sendErr = fmt.Errorf("send event %d: %w", ids[i], err)
			}
		}
		if len(sent) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM outbox WHERE id = ANY($1)`, sent); err != nil {
			return fmt.Errorf("delete sent events: %w", err)
		}
		return nil
	})
	if err != nil {
		// Whatever was sent stays in the outbox, to be sent again: at least once.
		return 0, err
	}
	r.metrics.sent.Add(float64(len(sent)))
	return len(sent), sendErr
}

func (r *Relay) measureEvery(ctx context.Context) {
	for {
		if err := r.measure(ctx); err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "outbox backlog", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(measureInterval):
		}
	}
}

// maxCounted is as far as measure counts the backlog, so measuring a huge
// backlog costs no more than measuring this many.
const maxCounted = 10_000

// measure sets the backlog metrics: how many events are waiting (up to
// maxCounted), and how long ago the oldest was added. Both read the primary
// key's index, not the whole table.
func (r *Relay) measure(ctx context.Context) error {
	var count int
	var oldest *time.Time
	if err := r.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM (SELECT 1 FROM outbox LIMIT $1) w),
		        (SELECT created_at FROM outbox ORDER BY id LIMIT 1)`, maxCounted).Scan(&count, &oldest); err != nil {
		return err
	}
	r.metrics.waiting.Set(float64(count))
	if oldest == nil {
		r.metrics.oldest.Set(0)
	} else {
		r.metrics.oldest.Set(time.Since(*oldest).Seconds())
	}
	return nil
}

// metrics say whether events are getting through. No labels: each service
// has its own outbox, and its metrics carry its name.
type metrics struct {
	waiting prometheus.Gauge
	oldest  prometheus.Gauge
	sent    prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		waiting: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_waiting_events",
			Help: "Events saved in the outbox and not yet sent to Kafka, counted up to 10000.",
		}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_oldest_waiting_seconds",
			Help: "How long ago the oldest waiting event was saved, or 0 if none is waiting.",
		}),
		sent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_sent_events_total",
			Help: "Events sent to Kafka and deleted from the outbox.",
		}),
	}
	reg.MustRegister(m.waiting, m.oldest, m.sent)
	return m
}
