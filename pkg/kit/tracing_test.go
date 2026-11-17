package kit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"google.golang.org/protobuf/types/known/apipb"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// recordSpans turns tracing on for one test, keeping every finished span in
// memory, and puts the previous global tracer provider and propagator back
// afterwards. It's kittest.RecordSpans, which tests elsewhere use; this
// package's own tests can't import kittest, which imports kit.
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
	if s.SpanKind != oteltrace.SpanKindServer || s.Name != "GET /things/:id" {
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

// spanNamed returns the one finished span called name.
func spanNamed(t *testing.T, exporter *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d spans named %q, want 1, among %d", len(found), name, len(exporter.GetSpans()))
	}
	return found[0]
}

// callSpanTo returns the one finished client span for a call to base's
// server, found by its port, as every test server is on 127.0.0.1.
func callSpanTo(t *testing.T, exporter *tracetest.InMemoryExporter, base string) tracetest.SpanStub {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	var found []tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		if s.SpanKind == oteltrace.SpanKindClient && attrs(s)["server.port"].AsInt64() == int64(port) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d client spans to port %d, want 1", len(found), port)
	}
	return found[0]
}

// TestACallJoinsTheTrace: a request handler calls another service. The
// call's client span sits under the handler's span, and the other service's
// server span under the call's, all in one trace: one request, one timeline.
func TestACallJoinsTheTrace(t *testing.T) {
	exporter := recordSpans(t)
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/accounts/:id", func(req typhon.Request) typhon.Response {
			return Respond(req, &apipb.Mixin{Name: "ok"}, http.StatusOK)
		})
	})

	ctx, handling := otel.Tracer("test").Start(t.Context(), "POST /transfers")
	if err := Get(ctx, base+"/accounts/a11ce000-0000-4000-8000-000000000001", &apipb.Mixin{}); err != nil {
		t.Fatal(err)
	}
	handling.End()

	client := callSpanTo(t, exporter, base)
	server := spanNamed(t, exporter, "GET /accounts/:id")
	trace := handling.SpanContext().TraceID()
	if client.SpanContext.TraceID() != trace || server.SpanContext.TraceID() != trace {
		t.Fatalf("spans in traces %s and %s, want both in the handler's trace %s", client.SpanContext.TraceID(), server.SpanContext.TraceID(), trace)
	}
	if client.Parent.SpanID() != handling.SpanContext().SpanID() || server.Parent.SpanID() != client.SpanContext.SpanID() {
		t.Error("want handler → call → other service's server span, each under the one before")
	}
	a := attrs(client)
	if client.Name != "GET 127.0.0.1" || a["http.request.method"].AsString() != "GET" ||
		a["server.address"].AsString() != "127.0.0.1" || a["http.response.status_code"].AsInt64() != 200 || client.Status.Code == codes.Error {
		t.Errorf("client span = %q %v %v, want GET 127.0.0.1 (host, no port) with method, server and status 200", client.Name, client.Attributes, client.Status)
	}
	if strings.Contains(client.Name+attributeText(client), "a11ce000") {
		t.Error("the client span carries the path's account ID; name it by method and service only")
	}
}

func TestAFailedCallMarksItsSpan(t *testing.T) {
	exporter := recordSpans(t)
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/thing", func(req typhon.Request) typhon.Response {
			return typhon.Response{Error: terrors.NotFound("thing", "no such thing", nil)}
		})
	})
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens there now

	_ = Get(t.Context(), base+"/thing", &apipb.Mixin{})
	_ = Get(t.Context(), down.URL+"/thing", &apipb.Mixin{})

	refused := attrs(callSpanTo(t, exporter, base))
	if refused["error.type"].AsString() != "not_found.thing" || refused["http.response.status_code"].AsInt64() != 404 {
		t.Errorf("refused call = %v, want error.type not_found.thing and status 404", refused)
	}
	unreachable := callSpanTo(t, exporter, down.URL)
	a := attrs(unreachable)
	if _, hasStatus := a["http.response.status_code"]; unreachable.Status.Code != codes.Error || hasStatus || a["error.type"].AsString() != "_OTHER" {
		t.Errorf("unreachable call = %v %v, want failed, error.type _OTHER and no status code (no response came back), not the internal_service Typhon makes up", a, unreachable.Status)
	}
}
