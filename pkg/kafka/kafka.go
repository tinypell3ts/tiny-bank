// Package kafka sends and receives messages on Kafka for tiny-bank's
// services, on franz-go. It's a thin layer that makes the choices
// every service should share:
//
//   - A message is sent only once Kafka has stored it (Producer.Produce
//     waits), and the producer is idempotent, so a retry inside franz-go
//     never writes a message twice.
//   - Messages with the same key go to the same partition, in order, so
//     everything about one account (say) is handled in the order it was sent.
//   - The trace travels in a traceparent header, so a request's trace
//     continues through Kafka.
package kafka

import (
	"context"
	"fmt"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// BrokersSetting is the setting that says where Kafka is: a comma-separated
// list of host:port, tiny-bank-kafka-bootstrap.kafka:9092 on the cluster.
const BrokersSetting = "KAFKA_BROKERS"

// Brokers reads BrokersSetting from getenv. It's required: a service that
// sends or receives messages refuses to start without it.
func Brokers(getenv func(string) string) ([]string, error) {
	var brokers []string
	for b := range strings.SplitSeq(getenv(BrokersSetting), ",") {
		if b = strings.TrimSpace(b); b != "" {
			brokers = append(brokers, b)
		}
	}
	if len(brokers) == 0 {
		return nil, fmt.Errorf("%s is required, like tiny-bank-kafka-bootstrap.kafka:9092", BrokersSetting)
	}
	return brokers, nil
}

var tracer = obs.NewTracer("github.com/tinypell3ts/tiny-bank/pkg/kafka")

// Message is one message received from Kafka.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
}

// Producer sends messages. It's safe to use from many goroutines.
type Producer struct{ client *kgo.Client }

// NewProducer returns a Producer for the Kafka at brokers. It connects
// lazily, on the first message. Close it on shutdown.
func NewProducer(brokers []string) (*Producer, error) {
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return nil, fmt.Errorf("kafka producer: %w", err)
	}
	return &Producer{client: client}, nil
}

// Produce sends value to topic, keyed by key, and returns once Kafka has
// stored it (or ctx ends). Messages with the same key keep their order. The
// span in ctx travels with the message as a traceparent header.
func (p *Producer) Produce(ctx context.Context, topic string, key, value []byte) error {
	ctx, span := tracer.Start(ctx, "send "+topic, trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(messagingAttrs(topic)...))
	defer span.End()

	record := &kgo.Record{Topic: topic, Key: key, Value: value}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier{record})
	if err := p.client.ProduceSync(ctx, record).FirstErr(); err != nil {
		obs.FailSpan(span, "_OTHER")
		return fmt.Errorf("send to %s: %w", topic, err)
	}
	span.SetAttributes(attribute.Int("messaging.destination.partition.id", int(record.Partition)))
	return nil
}

// Close waits for messages still being sent, then disconnects.
func (p *Producer) Close() { p.client.Close() }

func messagingAttrs(topic string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("messaging.system", "kafka"),
		attribute.String("messaging.destination.name", topic),
	}
}

// headerCarrier lets the trace propagator read and write a record's headers.
type headerCarrier struct{ r *kgo.Record }

var _ propagation.TextMapCarrier = headerCarrier{}

func (c headerCarrier) Get(key string) string {
	for _, h := range c.r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c headerCarrier) Set(key, value string) {
	for i, h := range c.r.Headers {
		if h.Key == key {
			c.r.Headers[i].Value = []byte(value)
			return
		}
	}
	c.r.Headers = append(c.r.Headers, kgo.RecordHeader{Key: key, Value: []byte(value)})
}

func (c headerCarrier) Keys() []string {
	keys := make([]string, len(c.r.Headers))
	for i, h := range c.r.Headers {
		keys[i] = h.Key
	}
	return keys
}
