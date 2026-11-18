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

// consume runs Consume in the background until the test ends, or until
// the stop it returns is called, which waits for Consume to return.
func consume(t *testing.T, brokers []string, group, topic string, handle Handler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Consume(ctx, brokers, group, []string{topic}, handle, discard); err != nil {
			t.Error(err)
		}
	}()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
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

// TestAGroupSharesTheWork: two members of one group split the partitions,
// and when a partition moves from one to the other, the first stops
// handling it before the second starts, so no message succeeds twice.
func TestAGroupSharesTheWork(t *testing.T) {
	brokers := newKafka(t, "transfers", 4)
	var sent [][2]string
	for i := range 40 {
		sent = append(sent, [2]string{fmt.Sprintf("account-%d", i), fmt.Sprint(i)})
	}
	produce(t, brokers, "transfers", sent...)

	// Both members fail every message until each has been offered one, so
	// whichever joins first is still retrying when its partitions move.
	var a, b, succeeded handled
	member := func(me *handled) Handler {
		return func(_ context.Context, m Message) error {
			me.add(m)
			if len(a.all()) == 0 || len(b.all()) == 0 {
				return errors.New("waiting for the other member")
			}
			succeeded.add(m)
			return nil
		}
	}
	consume(t, brokers, "share", "transfers", member(&a))
	eventually(t, "the first member retrying", func() bool { return len(a.all()) > 0 })
	consume(t, brokers, "share", "transfers", member(&b))

	eventually(t, "every message handled", func() bool { return len(succeeded.all()) >= 40 })
	time.Sleep(2 * retryPause) // a member still handling a partition it lost would show by now
	seen := map[string]int{}
	for _, m := range succeeded.all() {
		seen[string(m.Value)]++
	}
	for value, n := range seen {
		if n > 1 {
			t.Errorf("message %s succeeded %d times, want once", value, n)
		}
	}
	if len(seen) != 40 {
		t.Errorf("%d distinct messages handled, want 40", len(seen))
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

// TestAStuckPartitionDoesntHoldUpTheOthers: while one partition's message
// keeps failing, the other partitions' messages are still handled.
func TestAStuckPartitionDoesntHoldUpTheOthers(t *testing.T) {
	brokers := newKafka(t, "transfers", 2)
	var sent [][2]string
	for i := range 20 {
		sent = append(sent, [2]string{fmt.Sprintf("account-%d", i), fmt.Sprint(i)})
	}
	produce(t, brokers, "transfers", sent...)
	onPartition1 := 0
	for _, r := range readAll(t, brokers, "transfers", 20) {
		if r.Partition == 1 {
			onPartition1++
		}
	}

	var succeeded handled
	consume(t, brokers, "stuck", "transfers", func(_ context.Context, m Message) error {
		if m.Partition == 0 {
			return errors.New("ledger down")
		}
		succeeded.add(m)
		return nil
	})
	eventually(t, "partition 1 handled while partition 0 is stuck", func() bool { return len(succeeded.all()) == onPartition1 })
}

// TestAStoppingMemberCommitsWhatItHandled: when a member stops (a deploy)
// just as a handler succeeds, that message is committed, and the next
// member carries on after it: nothing lost, and nothing handled twice just
// because of the deploy.
func TestAStoppingMemberCommitsWhatItHandled(t *testing.T) {
	brokers := newKafka(t, "transfers", 1)
	produce(t, brokers, "transfers", [2]string{"alice", "one"}, [2]string{"alice", "two"}, [2]string{"alice", "three"})

	var first handled
	stop := consume(t, brokers, "redeliver", "transfers", func(ctx context.Context, m Message) error {
		first.add(m)
		if string(m.Value) == "two" {
			<-ctx.Done() // still working when the deploy starts, then done
		}
		return nil
	})
	eventually(t, "two offered", func() bool { return first.values() == "one,two" })
	stop()
	if first.values() != "one,two" {
		t.Fatalf("the stopping member handled %s, want one,two", first.values())
	}

	var second handled
	consume(t, brokers, "redeliver", "transfers", func(_ context.Context, m Message) error { second.add(m); return nil })
	eventually(t, "the next member handling", func() bool { return second.values() != "" })
	if second.values() != "three" {
		t.Errorf("the next member handled %s, want only three", second.values())
	}
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
