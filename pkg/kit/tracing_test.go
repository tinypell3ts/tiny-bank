package kit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordSpans turns tracing on for one test, keeping every finished span in
// memory, and puts the previous global tracer provider and propagator back
// afterwards.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	prevProvider, prevPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})
	return exporter
}

// A traceparent from a caller: trace 4bf9…4736, the caller's span 00f0…02b7.
const (
	callerTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpanID      = "00f067aa0ba902b7"
	callerTraceparent = "00-" + callerTraceID + "-" + callerSpanID + "-01"
)

func onlySpan(t *testing.T, exporter *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %v", len(spans), spans)
	}
	return spans[0]
}

func attrs(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

func TestASpanContinuesTheCallersTrace(t *testing.T) {
	exporter := recordSpans(t)
	h, _ := testServer(t)
	do(t, h, "/things/a11ce000-0000-4000-8000-000000000001", http.Header{"Traceparent": {callerTraceparent}})

	s := onlySpan(t, exporter)
	if s.SpanContext.TraceID().String() != callerTraceID || s.Parent.SpanID().String() != callerSpanID || !s.Parent.IsRemote() {
		t.Errorf("span is in trace %s under %s, want the caller's trace %s under its span %s",
			s.SpanContext.TraceID(), s.Parent.SpanID(), callerTraceID, callerSpanID)
	}
	if s.SpanKind != trace.SpanKindServer || s.Name != "GET /things/:id" {
		t.Errorf("span = %s %q, want a server span named by route pattern", s.SpanKind, s.Name)
	}
	a := attrs(s)
	if a["http.request.method"].AsString() != "GET" || a["http.route"].AsString() != "/things/:id" || a["http.response.status_code"].AsInt64() != 200 {
		t.Errorf("attributes = %v, want method, route pattern and status", s.Attributes)
	}
	if strings.Contains(s.Name+attributeText(s), "a11ce000") {
		t.Error("the span carries the raw path; routes must be patterns, like the metrics")
	}
	if s.Status.Code == codes.Error {
		t.Error("a 200 is marked as failed")
	}
}

func TestASpanWithNoCallerStartsANewTrace(t *testing.T) {
	exporter := recordSpans(t)
	h, _ := testServer(t)
	do(t, h, "/things/1", nil)
	if s := onlySpan(t, exporter); !s.SpanContext.HasTraceID() || s.Parent.HasTraceID() {
		t.Errorf("span = %v under %v, want the root of a new trace", s.SpanContext, s.Parent)
	}
}

func TestOnlyServerErrorsMarkASpanFailed(t *testing.T) {
	tests := []struct {
		path, route, code string
		status            int64
		failed            bool
	}{
		{"/missing", "/missing", "not_found.thing", 404, false},      // the caller's mistake
		{"/no-such-route", "unmatched", "not_found", 404, false},     // never a raw path
		{"/internal", "/internal", "internal_service.db", 500, true}, // ours
		{"/panic", "/panic", "internal_service.panic", 500, true},    // recovered, route still known
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			exporter := recordSpans(t)
			h, _ := testServer(t)
			do(t, h, tt.path, nil)
			s := onlySpan(t, exporter)
			a := attrs(s)
			if a["http.route"].AsString() != tt.route || a["http.response.status_code"].AsInt64() != tt.status ||
				!strings.HasPrefix(a["error.type"].AsString(), tt.code) || (s.Status.Code == codes.Error) != tt.failed {
				t.Errorf("span = %q %v, status %v; want route %s, %d, error.type %s, failed %v",
					s.Name, s.Attributes, s.Status, tt.route, tt.status, tt.code, tt.failed)
			}
		})
	}
}

func TestProbesGetNoSpans(t *testing.T) {
	exporter := recordSpans(t)
	h, _ := testServer(t)
	do(t, h, "/healthz", nil)
	do(t, h, "/readyz", nil)
	if spans := exporter.GetSpans(); len(spans) != 0 {
		t.Errorf("probes made %d spans, want none", len(spans))
	}
}

func TestAnUnknownMethodIsOther(t *testing.T) {
	exporter := recordSpans(t)
	h, _ := testServer(t)
	req, _ := http.NewRequestWithContext(t.Context(), "CUSTOM1", "/things/1", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if s := onlySpan(t, exporter); s.Name != "_OTHER unmatched" || attrs(s)["http.request.method"].AsString() != "_OTHER" {
		t.Errorf("span = %q %v, want the method as _OTHER, as in the metrics", s.Name, s.Attributes)
	}
}

func TestTheAccessLogCarriesTheTraceID(t *testing.T) {
	exporter := recordSpans(t)
	h, logs := testServer(t)
	do(t, h, "/things/1", http.Header{"Traceparent": {callerTraceparent}})
	var line struct {
		Msg     string `json:"msg"`
		TraceID string `json:"trace_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil || line.Msg != "http request" {
		t.Fatalf("access log = %s, %v", logs, err)
	}
	if line.TraceID != callerTraceID || onlySpan(t, exporter).SpanContext.TraceID().String() != line.TraceID {
		t.Errorf("access log trace_id = %q, want the span's trace %s", line.TraceID, callerTraceID)
	}
}

func attributeText(s tracetest.SpanStub) string {
	var b strings.Builder
	for _, kv := range s.Attributes {
		b.WriteString(kv.Value.String())
	}
	return b.String()
}
