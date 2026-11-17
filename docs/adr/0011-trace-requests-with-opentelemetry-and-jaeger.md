# 11. Trace requests with OpenTelemetry, sent to Jaeger

- **Status:** Accepted
- **Date:** 2026-11-17

## Context

A transfer crosses three services (transaction → ledger → account) and their databases. Metrics ([ADR-0003](0003-measure-services-with-prometheus-red-metrics.md)) say a route is slow or failing, and the request ID puts one request's log lines side by side, but neither says where the time went: which hop, which query, which lock. A trace does: one request as a timeline of spans, one per hop and query, joined by a trace ID that travels with each call.

Constraints:

- Every service traces the same way, without each one setting it up, like the metrics.
- Spans must hold no personal data, and a caller must not be able to create unlimited span names, for the same reasons as metric labels.
- It has to run on the local kind cluster today and look like what production would run.
- A request must never wait for, or fail because of, tracing.

## Decision

**OpenTelemetry for producing traces, Jaeger for receiving and showing them.**

- **The OpenTelemetry Go SDK**, set up once in [`pkg/obs`](../../pkg/obs/tracing.go) (`obs.StartTracing`) and started by `kit.Main`, so every service gets it. Spans are batched in memory and sent in the background over **OTLP/HTTP**. Every span is kept (parent-based, always on): sampling can come once there's enough traffic to need it.
- **Trace context travels as the W3C `traceparent` header**, the standard every tracing tool reads.
- **On only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set**, the standard setting name. The chart sets it to `http://jaeger.jaeger:4318` for every service. Tests and `go run` leave it unset, and tracing is a no-op. A malformed endpoint stops the service at startup.
- **Jaeger v2** on the cluster (`make jaeger`): all-in-one, built on the OpenTelemetry Collector, with traces kept in memory and its UI on localhost only.
- **Spans come from our own filters**, not OpenTelemetry's `otelhttp` middleware: a server span in `kit.Handler` named by the Typhon route pattern (`POST /transfers`), client spans in `kit.Get` and `kit.Post`, and database spans from a small pgx tracer of our own that records the SQL text but never its parameters.

New direct dependencies, pinned in `go.mod`: `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/trace` (both already there indirectly, through testcontainers), `go.opentelemetry.io/otel/sdk` and `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`, all v1.47.0.

Alternatives we didn't choose:

- **Grafana Tempo instead of Jaeger.** It sits next to our Grafana, but needs object storage and more setup, and Jaeger's own UI is the quickest way to follow one trace. Since services speak OTLP, swapping the receiver later is a setting, not a code change.
- **Zipkin or Jaeger's own client libraries.** Older formats; both projects now point to OpenTelemetry.
- **OTLP over gRPC.** Equally standard, but HTTP is simpler to debug (`curl` can post a span) and our services already speak HTTP.
- **`otelhttp` middleware for spans.** It names spans from the raw path unless told otherwise, and records more attributes than we choose. Our own Typhon filter knows the route pattern and uses the same fixed attributes as the metrics.
- **`otelpgx` for database spans.** Another dependency, for about 60 lines we can write and control (no query parameters, ever).

## Consequences

- One request can be followed across every service and query in the Jaeger UI, by its trace ID, which also goes in each log line.
- **Each binary grows by about 6 MB** (`hello` from 9.4 to 15.6 MB, the ledger from 14.2 to 20.1 MB). The OTLP exporter's protobuf package carries the collector's gRPC stubs too, so `google.golang.org/grpc` and `grpc-gateway` are compiled in even though we only send over HTTP. We accept that for the official, maintained exporter; writing our own would be more code to keep correct.
- Without Jaeger installed, spans fail to send and are dropped, with a warning in the logs. Requests aren't affected.
- Traces in memory are lost when Jaeger restarts. Fine locally; a shared environment would need storage.
- Keeping every span is fine at local traffic. Under real load, sampling decides which traces to keep, and that's a decision for then.
- At shutdown, `kit.Main` waits up to 3 seconds to send the last spans, inside the 5 seconds the chart keeps spare after the server stops.

## Security

- **New dependencies:** the four modules above and what they pull in (the OTLP protobufs, gRPC, grpc-gateway, `cenkalti/backoff`). All pinned in `go.mod`. v1.47.0 includes the 2026 fix for OTLP HTTP exporters reading unbounded response bodies (CVE-2026-39883).
- **New outbound traffic:** every service sends spans to Jaeger, in plain HTTP inside the cluster, under the existing gap "No TLS between services". Jaeger is a ClusterIP Service, and its UI, which has no login, is reachable only through a localhost port-forward ([docs/security.md](../security.md)).
- **What spans may hold** follows the metric-label rule: route patterns, methods, status codes, error codes and SQL text without parameters; never names, customer IDs, amounts or anything a caller typed. Account and transfer IDs are random UUIDs and may appear, as in logs.
- **Trusting an incoming `traceparent`:** a service continues whatever trace its caller sends. That's safe while nothing outside the cluster can reach a service; the edge gateway, once there is one, must start a new trace for every outside request rather than continue one a client sent.
- The endpoint setting isn't a secret, but it's never repeated in a startup error, in case one ever carries credentials in its URL.
