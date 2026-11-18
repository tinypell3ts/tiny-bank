// Package kafka sends and receives messages on Kafka for tiny-bank's
// services, on franz-go. It's a thin layer that makes the choices
// every service should share:
//
//   - A message is sent only once Kafka has stored it (Producer.Produce
//     waits), and the producer is idempotent, so a retry inside franz-go
//     never writes a message twice.
//   - A message is received at least once (Consume): its offset is committed
//     only after the handler succeeds, so a crash or a failed handler means
//     it's handled again, never skipped. Handlers must cope with seeing a
//     message twice.
//   - Messages with the same key go to the same partition, in order, so
//     everything about one account (say) is handled in the order it was sent.
//   - The trace travels in a traceparent header, so a request's trace
//     continues through Kafka into whatever handles the message.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/monzo/terrors"
	"github.com/twmb/franz-go/pkg/kerr"
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

// Produce sends value to topic, keyed by key, and returns nil once Kafka has
// stored it. Messages with the same key keep their order. The span in ctx
// travels with the message as a traceparent header.
//
// An error doesn't always mean the message wasn't stored: if ctx ends while
// it's on its way, Kafka may store it anyway. A caller that sends it again
// may send it twice, which consumers must cope with in any case.
func (p *Producer) Produce(ctx context.Context, topic string, key, value []byte) error {
	ctx, span := tracer.Start(ctx, "send "+topic, trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(messagingAttrs(topic)...))
	defer span.End()

	record := &kgo.Record{Topic: topic, Key: key, Value: value}
	otel.GetTextMapPropagator().Inject(ctx, headerCarrier{record})
	if err := p.client.ProduceSync(ctx, record).FirstErr(); err != nil {
		obs.FailSpan(span, errorType(err))
		return fmt.Errorf("send to %s: %w", topic, err)
	}
	span.SetAttributes(attribute.Int("messaging.destination.partition.id", int(record.Partition)))
	return nil
}

// Close waits for messages still being sent, then disconnects.
func (p *Producer) Close() { p.client.Close() }

// errorType names err for a span's error.type: Kafka's own error code, like
// NOT_ENOUGH_REPLICAS, or _OTHER (a timeout, say). A handler's error may
// also be a terror, named by its code.
func errorType(err error) string {
	if kafkaErr := (*kerr.Error)(nil); errors.As(err, &kafkaErr) {
		return kafkaErr.Message
	}
	if terr := (*terrors.Error)(nil); errors.As(err, &terr) {
		return terr.Code
	}
	return "_OTHER"
}

// Handler handles one message. Returning an error means "not handled": the
// message is offered again (after a pause) and its offset isn't committed,
// so nothing after it on its partition is handled until it succeeds.
//
// A handler must stop promptly once ctx ends, as that's how it learns its
// partition is moving to another member, and it must cope with seeing a
// message twice.
type Handler func(ctx context.Context, m Message) error

const (
	// retryPause is how long Consume waits before offering a message whose
	// handler failed again, or fetching again after a fetch failed.
	retryPause = time.Second
	// commitTimeout bounds a commit, which runs even as Consume stops.
	commitTimeout = 5 * time.Second
)

// Consume receives messages from topics as part of consumer group group,
// and calls handle for each, until ctx ends. The group's members share the
// topics' partitions between them, so running more pods spreads the work,
// and when one stops, its partitions move to the others.
//
// Each partition has a worker of its own, handling its messages in order.
// A message's offset is committed only once handle succeeds, so every
// message is handled at least once. A message whose handler fails is offered
// again, every retryPause, until it succeeds or its partition moves away:
// that partition waits, keeping order, while the others carry on. Before a
// partition is handed to another member, its worker stops and commits what
// it handled, so in an orderly handoff (a rebalance or a deploy) two members
// don't handle one partition at once, as long as handlers stop promptly when
// their ctx ends. It can't prevent that when a member loses contact with
// Kafka: the group gives its partitions away when its session expires, while
// a slow handler may still be running. Handlers that write must be safe
// even then, the way the ledger's idempotency keys are. Handler errors are
// logged to logger.
func Consume(ctx context.Context, brokers []string, group string, topics []string, handle Handler, logger *slog.Logger) error {
	c := &consumer{ctx: ctx, handle: handle, logger: logger, workers: map[topicPartition]*worker{}}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(), // workers commit what they've handled
		// If a broker loses messages it had accepted, a member can't tell
		// exactly where to resume. franz-go's default goes back a minute
		// before the last message it read, which could skip one that
		// survived; we start the partition again from its oldest message.
		// That re-reads up to the topic's retention, which handlers must cope
		// with anyway. (A new group, or an offset older than the oldest
		// message, starts at the oldest message either way.)
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(c.start),
		kgo.OnPartitionsRevoked(c.stop),
		kgo.OnPartitionsLost(c.stop),
	)
	if err != nil {
		return fmt.Errorf("kafka consumer: %w", err)
	}
	defer client.Close() // leaves the group: c.stop runs for every partition first

	for {
		fetches := client.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, fe := range errs {
				logger.ErrorContext(ctx, "kafka fetch", "topic", fe.Topic, "partition", fe.Partition, "error", fe.Err)
			}
			select { // don't spin on an error that won't go away
			case <-ctx.Done():
				return nil
			case <-time.After(retryPause):
			}
		}
		fetches.EachPartition(func(p kgo.FetchTopicPartition) { c.dispatch(client, p) })
	}
}

type topicPartition struct {
	topic     string
	partition int32
}

// consumer keeps one worker per partition this member is assigned.
type consumer struct {
	ctx     context.Context
	handle  Handler
	logger  *slog.Logger
	mu      sync.Mutex
	workers map[topicPartition]*worker
}

// worker handles one partition's messages, a batch at a time.
type worker struct {
	ctx     context.Context
	cancel  context.CancelFunc
	batches chan []*kgo.Record
	done    chan struct{}
}

func (c *consumer) start(_ context.Context, client *kgo.Client, assigned map[string][]int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for topic, partitions := range assigned {
		for _, p := range partitions {
			ctx, cancel := context.WithCancel(c.ctx)
			w := &worker{ctx: ctx, cancel: cancel, batches: make(chan []*kgo.Record, 1), done: make(chan struct{})}
			c.workers[topicPartition{topic, p}] = w
			go c.run(client, w)
		}
	}
}

// stop stops the workers of partitions moving away (or the client closing)
// and waits for each to commit what it handled: in a rebalance, the group
// doesn't give a partition to its new member until this returns. For lost
// partitions it's too late for that (they may already have a new member),
// but it still stops their workers.
func (c *consumer) stop(_ context.Context, client *kgo.Client, partitions map[string][]int32) {
	c.mu.Lock()
	var stopping []*worker
	for topic, ps := range partitions {
		for _, p := range ps {
			if w, ok := c.workers[topicPartition{topic, p}]; ok {
				w.cancel()
				stopping = append(stopping, w)
				delete(c.workers, topicPartition{topic, p})
			}
		}
	}
	c.mu.Unlock()
	for _, w := range stopping {
		<-w.done
	}
	client.ResumeFetchPartitions(partitions) // so it isn't paused if it comes back
}

// dispatch hands a partition's fetched messages to its worker, and pauses
// fetching that partition until the worker is done with them, so the poll
// loop never waits on a slow or failing partition.
func (c *consumer) dispatch(client *kgo.Client, p kgo.FetchTopicPartition) {
	if len(p.Records) == 0 {
		return
	}
	c.mu.Lock()
	w, ok := c.workers[topicPartition{p.Topic, p.Partition}]
	if ok {
		client.PauseFetchPartitions(map[string][]int32{p.Topic: {p.Partition}})
	}
	c.mu.Unlock()
	if !ok {
		return // moved away since it was fetched: its new member fetches it again
	}
	select {
	case w.batches <- p.Records:
	case <-w.ctx.Done():
	}
}

// run handles each batch in order, commits what it handled, and resumes
// fetching, until its partition moves away.
func (c *consumer) run(client *kgo.Client, w *worker) {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case batch := <-w.batches:
			handled := 0
			for _, record := range batch {
				if !handleUntilDone(w.ctx, record, c.handle, c.logger) {
					break
				}
				handled++
			}
			c.commit(client, batch[:handled])
			if handled < len(batch) {
				return // stopped: what's left is handled by whoever gets the partition
			}
			client.ResumeFetchPartitions(map[string][]int32{batch[0].Topic: {batch[0].Partition}})
		}
	}
}

// commit commits the offset after the last of handled, even if Consume is
// stopping: what's handled shouldn't be handled again just because of a
// deploy.
func (c *consumer) commit(client *kgo.Client, handled []*kgo.Record) {
	if len(handled) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), commitTimeout)
	defer cancel()
	last := handled[len(handled)-1]
	if err := client.CommitRecords(ctx, last); err != nil {
		// Handled but not committed: it'll be handled again, which
		// at-least-once allows.
		c.logger.ErrorContext(ctx, "kafka commit", "topic", last.Topic, "partition", last.Partition, "error", err)
	}
}

// handleUntilDone calls handle for record in a consumer span that continues
// the producer's trace, until it succeeds (true) or ctx ends (false). Once
// ctx has ended, it doesn't start.
func handleUntilDone(ctx context.Context, record *kgo.Record, handle Handler, logger *slog.Logger) bool {
	parent := otel.GetTextMapPropagator().Extract(ctx, headerCarrier{record})
	m := Message{Topic: record.Topic, Partition: record.Partition, Offset: record.Offset, Key: record.Key, Value: record.Value}
	for ctx.Err() == nil {
		spanCtx, span := tracer.Start(parent, "process "+record.Topic, trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(append(messagingAttrs(record.Topic),
				attribute.Int("messaging.destination.partition.id", int(record.Partition)),
				attribute.Int64("messaging.kafka.offset", record.Offset))...))
		err := handle(spanCtx, m)
		if err != nil {
			obs.FailSpan(span, errorType(err))
			logger.ErrorContext(spanCtx, "handle kafka message, will retry", "topic", record.Topic,
				"partition", record.Partition, "offset", record.Offset, "error", err)
		}
		span.End()
		if err == nil {
			return true
		}
		select {
		case <-ctx.Done():
		case <-time.After(retryPause):
		}
	}
	return false
}

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
