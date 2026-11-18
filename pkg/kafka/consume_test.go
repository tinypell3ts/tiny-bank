package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

var discard = slog.New(slog.DiscardHandler)

func produce(t *testing.T, brokers []string, topic string, messages ...[2]string) {
	t.Helper()
	p, err := NewProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, m := range messages {
		if err := p.Produce(t.Context(), topic, []byte(m[0]), []byte(m[1])); err != nil {
			t.Fatal(err)
		}
	}
}

// consume runs Consume in the background until the test ends.
func consume(t *testing.T, brokers []string, group, topic string, handle Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Consume(ctx, brokers, group, []string{topic}, handle, discard); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// eventually waits up to 20s for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// handled records what a handler saw, safely across goroutines.
type handled struct {
	mu   sync.Mutex
	msgs []Message
}

func (h *handled) add(m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, m)
}

func (h *handled) all() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.msgs)
}

func (h *handled) values() string {
	var vs []string
	for _, m := range h.all() {
		vs = append(vs, string(m.Value))
	}
	return strings.Join(vs, ",")
}

// TestAGroupSharesTheWork: two members of one group split the partitions
// between them, and between them every message is handled.
func TestAGroupSharesTheWork(t *testing.T) {
	brokers := newKafka(t, "transfers", 4)
	var a, b handled
	consume(t, brokers, "share", "transfers", func(_ context.Context, m Message) error { a.add(m); return nil })
	consume(t, brokers, "share", "transfers", func(_ context.Context, m Message) error { b.add(m); return nil })
	time.Sleep(3 * time.Second) // let both join before anything is sent

	var sent [][2]string
	for i := range 40 {
		sent = append(sent, [2]string{fmt.Sprintf("account-%d", i), fmt.Sprint(i)})
	}
	produce(t, brokers, "transfers", sent...)

	eventually(t, "every message handled", func() bool {
		seen := map[string]bool{}
		for _, m := range append(a.all(), b.all()...) {
			seen[string(m.Value)] = true
		}
		return len(seen) == 40
	})
	if len(a.all()) == 0 || len(b.all()) == 0 {
		t.Errorf("one member handled everything (%d and %d), want the partitions shared", len(a.all()), len(b.all()))
	}
}

// TestAFailedMessageIsRetriedAndHoldsItsPartition: until a message is
// handled, the ones after it on its partition wait, so order holds.
func TestAFailedMessageIsRetriedAndHoldsItsPartition(t *testing.T) {
	brokers := newKafka(t, "transfers", 1)
	produce(t, brokers, "transfers", [2]string{"alice", "first"}, [2]string{"alice", "second"})

	var got handled
	failures := 2
	consume(t, brokers, "retry", "transfers", func(_ context.Context, m Message) error {
		got.add(m)
		if string(m.Value) == "first" && failures > 0 {
			failures--
			return errors.New("ledger down")
		}
		return nil
	})
	eventually(t, "both messages handled", func() bool { return strings.HasSuffix(got.values(), "second") })
	if got.values() != "first,first,first,second" {
		t.Errorf("handled %s, want first three times (two failures), then second", got.values())
	}
}

// TestAMessageNotCommittedIsHandledAgain: a consumer that handles a message
// but stops before committing it (a crash, a deploy) leaves it for the next
// member of the group: at least once, never lost.
func TestAMessageNotCommittedIsHandledAgain(t *testing.T) {
	brokers := newKafka(t, "transfers", 1)
	produce(t, brokers, "transfers", [2]string{"alice", "deposit"})

	ctx, stop := context.WithCancel(t.Context())
	var first handled
	if err := Consume(ctx, brokers, "redeliver", []string{"transfers"}, func(_ context.Context, m Message) error {
		first.add(m)
		stop() // "crash" after handling, before the commit
		return nil
	}, discard); err != nil {
		t.Fatal(err)
	}
	if first.values() != "deposit" {
		t.Fatalf("first consumer handled %q, want deposit", first.values())
	}

	var second handled
	consume(t, brokers, "redeliver", "transfers", func(_ context.Context, m Message) error { second.add(m); return nil })
	eventually(t, "the uncommitted message handled again", func() bool { return second.values() == "deposit" })
}

// TestTheTraceTravelsThroughKafka: the handler runs in a process span, in the
// producer's trace, under the span that sent the message.
func TestTheTraceTravelsThroughKafka(t *testing.T) {
	exporter := kittest.RecordSpans(t)
	brokers := newKafka(t, "transfers", 1)
	ctx, request := otel.Tracer("test").Start(t.Context(), "POST /transfers")
	p, err := NewProducer(brokers)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Produce(ctx, "transfers", []byte("alice"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	p.Close()
	request.End()

	var got handled
	var inHandler trace.SpanContext
	consume(t, brokers, "trace", "transfers", func(ctx context.Context, m Message) error {
		inHandler = trace.SpanContextFromContext(ctx)
		got.add(m)
		return nil
	})
	eventually(t, "the message handled", func() bool { return got.values() == "x" })

	spans := map[string]trace.SpanContext{}
	var processParent trace.SpanID
	for _, s := range exporter.GetSpans() {
		spans[s.Name] = s.SpanContext
		if s.Name == "process transfers" {
			processParent = s.Parent.SpanID()
		}
	}
	send, process := spans["send transfers"], spans["process transfers"]
	if !send.IsValid() || !process.IsValid() {
		t.Fatalf("spans = %v, want send transfers and process transfers", exporter.GetSpans())
	}
	if process.TraceID() != request.SpanContext().TraceID() || processParent != send.SpanID() || inHandler.SpanID() != process.SpanID() {
		t.Error("want request → send → process in one trace, with the handler running in the process span")
	}
}
