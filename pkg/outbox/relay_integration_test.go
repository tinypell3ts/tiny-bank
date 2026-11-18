//go:build integration

package outbox

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/tinypell3ts/tiny-bank/pkg/kafka"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// setup returns a migrated database, and an in-memory Kafka with topic
// "events" and a Relay sending to it.
func setup(t *testing.T) (*pgxpool.Pool, []string, *Relay) {
	t.Helper()
	pool := migrated(t)
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "events"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	return pool, cluster.ListenAddrs(), newRelay(t, pool, cluster.ListenAddrs())
}

func newRelay(t *testing.T, pool *pgxpool.Pool, brokers []string) *Relay {
	t.Helper()
	producer, err := kafka.NewProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	return NewRelay(pool, producer, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
}

// read reads n events from topic, as the strings they hold.
func read(t *testing.T, brokers []string, topic string, n int) ([]string, []*kgo.Record) {
	t.Helper()
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topic))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var records []*kgo.Record
	for len(records) < n && ctx.Err() == nil {
		records = append(records, client.PollFetches(ctx).Records()...)
	}
	var values []string
	for _, r := range records {
		var event wrapperspb.StringValue
		if err := proto.Unmarshal(r.Value, &event); err != nil {
			t.Fatal(err)
		}
		values = append(values, event.GetValue())
	}
	return values, records
}

// TestARelaySendsEventsInOrderAndDeletesThem, carrying the trace of the
// request that added each one.
func TestARelaySendsEventsInOrderAndDeletesThem(t *testing.T) {
	pool, brokers, relay := setup(t)
	exporter := kittest.RecordSpans(t)
	ctx, request := otel.Tracer("test").Start(t.Context(), "POST /transfers")
	add(t, ctx, pool, "events", "one", "two", "three")
	request.End()

	// They share a key, so they go one per batch, each after the one before.
	for range 3 {
		if sent, err := relay.SendBatch(t.Context()); sent != 1 || err != nil {
			t.Fatalf("SendBatch = %d, %v; want 1, nil", sent, err)
		}
	}
	values, records := read(t, brokers, "events", 3)
	if strings.Join(values, ",") != "one,two,three" {
		t.Errorf("sent %v, want one,two,three", values)
	}
	if n := waiting(t, pool); n != 0 {
		t.Errorf("%d events still in the outbox, want 0", n)
	}
	for _, s := range exporter.GetSpans() {
		if s.Name == "send events" && s.Parent.SpanID() != request.SpanContext().SpanID() {
			t.Error("a send span isn't under the request that added its event")
		}
	}
	if tp := string(records[0].Headers[0].Value); !strings.Contains(tp, request.SpanContext().TraceID().String()) {
		t.Errorf("traceparent = %q, want the request's trace", tp)
	}
	if got := testutil.ToFloat64(relay.metrics.sent); got != 3 {
		t.Errorf("outbox_sent_events_total = %v, want 3", got)
	}
}

// TestAnEventKafkaWontTakeHoldsBackOnlyItsKey: the events after it with
// the same key wait, so they never overtake it, however many there are;
// other keys' events go. It stays, counted in the backlog, until Kafka takes
// it.
func TestAnEventKafkaWontTakeHoldsBackOnlyItsKey(t *testing.T) {
	pool, brokers, relay := setup(t)
	events := [][2]string{{"alice", strings.Repeat("x", 2<<20)}} // over the producer's 1 MB limit
	for range batchSize {
		events = append(events, [2]string{"alice", "after"})
	}
	events = append(events, [2]string{"bob", "other"})
	for _, e := range events {
		if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			return Add(t.Context(), tx, "events", []byte(e[0]), wrapperspb.String(e[1]))
		}); err != nil {
			t.Fatal(err)
		}
	}

	if sent, err := relay.SendBatch(t.Context()); sent != 1 || err == nil {
		t.Fatalf("SendBatch = %d, %v; want 1 (bob's) and an error", sent, err)
	}
	if got, _ := read(t, brokers, "events", 2); strings.Join(got, ",") != "other" {
		t.Errorf("Kafka has %v, want only bob's event: alice's after must wait for her first", got)
	}
	if n := waiting(t, pool); n != batchSize+1 {
		t.Errorf("%d events in the outbox, want alice's %d", n, batchSize+1)
	}
	if err := relay.measure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(relay.metrics.waiting); got != batchSize+1 {
		t.Errorf("outbox_waiting_events = %v, want %d", got, batchSize+1)
	}
	if got := testutil.ToFloat64(relay.metrics.oldest); got <= 0 {
		t.Errorf("outbox_oldest_waiting_seconds = %v, want more than 0", got)
	}
}

// TestARelayWhoseKafkaIsGoneGivesUp: with no Kafka answering, SendBatch
// gives up by itself within sendTimeout, rolls back, keeping the event, and
// lets go of the relay lock, so another relay sends it.
func TestARelayWhoseKafkaIsGoneGivesUp(t *testing.T) {
	pool, _, working := setup(t)
	gone, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	stuck := newRelay(t, pool, gone.ListenAddrs())
	gone.Close()
	add(t, t.Context(), pool, "events", "waiting")

	start := time.Now()
	if sent, err := stuck.SendBatch(t.Context()); sent != 0 || err == nil {
		t.Fatalf("SendBatch = %d, %v; want 0 and an error", sent, err)
	}
	if took := time.Since(start); took < sendTimeout/2 || took > sendTimeout+5*time.Second {
		t.Errorf("SendBatch took %v, want it to wait about %v, then give up", took, sendTimeout)
	}
	if n := waiting(t, pool); n != 1 {
		t.Errorf("%d events in the outbox, want the one", n)
	}
	if sent, err := working.SendBatch(t.Context()); sent != 1 || err != nil {
		t.Errorf("another relay's SendBatch = %d, %v; want 1, nil (the lock let go)", sent, err)
	}
}

// TestRelaysOnManyPodsSendEachEventOnceInOrder: they take turns.
func TestRelaysOnManyPodsSendEachEventOnceInOrder(t *testing.T) {
	pool, brokers, _ := setup(t)
	var want []string
	for i := range 50 {
		want = append(want, string(rune('a'+i%26))+strings.Repeat("!", i/26))
	}
	add(t, t.Context(), pool, "events", want...)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range 3 {
		relay := newRelay(t, pool, brokers)
		wg.Go(func() { relay.Run(ctx) })
	}
	deadline := time.Now().Add(20 * time.Second)
	for waiting(t, pool) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	got, _ := read(t, brokers, "events", 50)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sent %v, want each event once, in order: %v", got, want)
	}
}
