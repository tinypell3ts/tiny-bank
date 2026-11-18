package kafka

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// newKafka starts an in-memory Kafka with topic, of partitions partitions.
func newKafka(t *testing.T, topic string, partitions int32) []string {
	t.Helper()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(partitions, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	return cluster.ListenAddrs()
}

// readAll reads n records from topic with franz-go directly.
func readAll(t *testing.T, brokers []string, topic string, n int) []*kgo.Record {
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
	if len(records) < n {
		t.Fatalf("read %d records, want %d", len(records), n)
	}
	return records
}

// TestProduceKeepsAKeyOnOnePartition: every message with one key lands on
// one partition, in the order it was sent.
func TestProduceKeepsAKeyOnOnePartition(t *testing.T) {
	brokers := newKafka(t, "transfers", 3)
	p, err := NewProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	keys := []string{"alice", "bob", "carol"}
	for i := range 30 {
		if err := p.Produce(t.Context(), "transfers", []byte(keys[i%3]), []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}

	partitionOf, last := map[string]int32{}, map[string]int{}
	for _, r := range readAll(t, brokers, "transfers", 30) {
		key, value := string(r.Key), int(r.Value[0])
		if p, ok := partitionOf[key]; ok && p != r.Partition {
			t.Errorf("key %s on partitions %d and %d, want one", key, p, r.Partition)
		}
		partitionOf[key] = r.Partition
		if prev, ok := last[key]; ok && value < prev {
			t.Errorf("key %s: %d stored after %d, want the order they were sent", key, value, prev)
		}
		last[key] = value
	}
}

// TestProduceSendsTheTrace: the message carries a traceparent naming the
// send span, which is under the caller's span.
func TestProduceSendsTheTrace(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	brokers := newKafka(t, "transfers", 1)
	p, err := NewProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, request := otel.Tracer("test").Start(t.Context(), "POST /transfers")
	if err := p.Produce(ctx, "transfers", []byte("alice"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	request.End()

	spans := exporter.GetSpans()
	i := slices.IndexFunc(spans, func(s tracetest.SpanStub) bool { return s.Name == "send transfers" })
	if i < 0 {
		t.Fatalf("spans = %v, want send transfers", spans)
	}
	send := spans[i]
	if send.Parent.SpanID() != request.SpanContext().SpanID() {
		t.Error("send span isn't under the caller's span")
	}
	record := readAll(t, brokers, "transfers", 1)[0]
	header := headerCarrier{record}.Get("traceparent")
	if !strings.Contains(header, send.SpanContext.TraceID().String()) || !strings.Contains(header, send.SpanContext.SpanID().String()) {
		t.Errorf("traceparent = %q, want the send span's trace and span IDs", header)
	}
}

// TestProduceFailsWhenKafkaWontStoreIt: an unknown topic (auto-creating
// topics is off) is an error once ctx ends, and a failed span.
func TestProduceFailsWhenKafkaWontStoreIt(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	p, err := NewProducer(newKafka(t, "transfers", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := p.Produce(ctx, "no-such-topic", nil, []byte("x")); err == nil {
		t.Fatal("Produce to an unknown topic: err = nil, want an error")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Status.Code != codes.Error {
		t.Errorf("spans = %v, want one failed send span", spans)
	}
}

// TestProduceAllSaysHowManyWereStoredInOrder: a message Kafka won't take
// stops the count there, even if later ones were stored.
func TestProduceAllSaysHowManyWereStoredInOrder(t *testing.T) {
	brokers := newKafka(t, "transfers", 1)
	p, err := NewProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stored, err := p.ProduceAll(ctx, []Outgoing{
		{Topic: "transfers", Value: []byte("one")},
		{Topic: "no-such-topic", Value: []byte("stuck")},
		{Topic: "transfers", Value: []byte("three")},
	})
	if stored != 1 || err == nil || !strings.Contains(err.Error(), "no-such-topic") {
		t.Errorf("ProduceAll = %d, %v; want 1 and an error about no-such-topic", stored, err)
	}
	if got := readAll(t, brokers, "transfers", 2); string(got[0].Value) != "one" || string(got[1].Value) != "three" {
		t.Errorf("stored %s, %s; want one, three", got[0].Value, got[1].Value)
	}
}

func TestErrorType(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("send: %w", kerr.NotEnoughReplicas): "NOT_ENOUGH_REPLICAS",
		context.DeadlineExceeded:                       "_OTHER",
	} {
		if got := errorType(err); got != want {
			t.Errorf("errorType(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestBrokers(t *testing.T) {
	for setting, want := range map[string][]string{
		"tiny-bank-kafka-bootstrap.kafka:9092": {"tiny-bank-kafka-bootstrap.kafka:9092"},
		"a:9092, b:9092,":                      {"a:9092", "b:9092"},
	} {
		got, err := Brokers(func(string) string { return setting })
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Brokers(%q) = %v, %v; want %v", setting, got, err, want)
		}
	}
	if _, err := Brokers(func(string) string { return " , " }); err == nil || !strings.Contains(err.Error(), BrokersSetting) {
		t.Errorf("an empty setting: err = %v, want one naming %s", err, BrokersSetting)
	}
}
