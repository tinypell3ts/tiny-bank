package obs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// receiver is a stand-in OTLP/HTTP receiver that keeps what it's sent.
type receiver struct {
	mu       sync.Mutex
	path     string
	ctype    string
	received []byte
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.path, r.ctype, r.received = req.URL.Path, req.Header.Get("Content-Type"), append(r.received, body...)
	r.mu.Unlock()
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestTracingSendsSpansToTheEndpoint(t *testing.T) {
	rcv := &receiver{}
	srv := httptest.NewServer(rcv)
	t.Cleanup(srv.Close)

	shutdown, err := StartTracing("ledger", env(map[string]string{TracingEndpoint: srv.URL + "/"}), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(t.Context(), "post journal")
	if !span.IsRecording() {
		t.Fatal("span isn't recording with an endpoint set")
	}
	span.End()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil { // sends what's buffered
		t.Fatal(err)
	}
	rcv.mu.Lock()
	defer rcv.mu.Unlock()
	if rcv.path != "/v1/traces" || rcv.ctype != "application/x-protobuf" {
		t.Errorf("sent to %q as %q, want /v1/traces as OTLP protobuf", rcv.path, rcv.ctype)
	}
	for _, want := range []string{"post journal", "service.name", "ledger"} {
		if !bytes.Contains(rcv.received, []byte(want)) {
			t.Errorf("export has no %q", want)
		}
	}
}

func TestTracingIsOffWithoutAnEndpoint(t *testing.T) {
	shutdown, err := StartTracing("ledger", env(nil), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, span := otel.Tracer("test").Start(t.Context(), "x"); span.IsRecording() {
		t.Error("span is recording with no endpoint, want tracing off")
	}
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("shutdown = %v, want nil", err)
	}
}

func TestTracingPassesTraceContextAsTraceparent(t *testing.T) {
	shutdown, err := StartTracing("ledger", env(map[string]string{TracingEndpoint: "http://127.0.0.1:1"}), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) }) // nothing listens there; the export just fails
	ctx, span := otel.Tracer("test").Start(t.Context(), "x")
	defer span.End()

	header := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
	traceID := span.SpanContext().TraceID().String()
	if got := header.Get("traceparent"); !strings.HasPrefix(got, "00-"+traceID+"-") {
		t.Errorf("traceparent = %q, want one carrying trace %s", got, traceID)
	}
}

func TestTracesURLAddsThePathBeforeAnyQuery(t *testing.T) {
	tests := map[string]string{
		"http://jaeger.jaeger:4318":        "http://jaeger.jaeger:4318/v1/traces",
		"http://jaeger.jaeger:4318/":       "http://jaeger.jaeger:4318/v1/traces",
		"https://collector/otel":           "https://collector/otel/v1/traces",
		"https://collector/otel/?key=k":    "https://collector/otel/v1/traces?key=k",
		"http://collector:4318?tenant=a&b": "http://collector:4318/v1/traces?tenant=a&b",
	}
	for endpoint, want := range tests {
		if got, err := tracesURLFrom(endpoint); err != nil || got != want {
			t.Errorf("tracesURLFrom(%q) = %q, %v; want %q", endpoint, got, err, want)
		}
	}
}

func TestTracingRefusesABadEndpoint(t *testing.T) {
	// None of these appear in the error's own example URL, so the check that
	// the value isn't repeated (it could hold credentials) means something.
	for _, bad := range []string{"collector-typo:4318", "ftp://collector-typo", "https://:4318", "://collector-typo"} {
		if _, err := StartTracing("ledger", env(map[string]string{TracingEndpoint: bad}), discardLogger()); err == nil ||
			!strings.Contains(err.Error(), TracingEndpoint) || strings.Contains(err.Error(), bad) {
			t.Errorf("%q: err = %v, want a startup error naming the setting, not repeating its value", bad, err)
		}
	}
}

func TestTracingErrorsAreLoggedAsWarnings(t *testing.T) {
	var logs bytes.Buffer
	if _, err := StartTracing("ledger", env(nil), slog.New(slog.NewJSONHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	otel.Handle(errors.New("export failed"))
	if got := logs.String(); !strings.Contains(got, `"level":"WARN"`) || !strings.Contains(got, "export failed") {
		t.Errorf("logs = %s, want the error as a JSON warning", got)
	}
}
