# Observability

A bank must be able to answer **"what happened to this customer's money, and why?"** at any time. Observability is built in from the start, one layer at a time, each arriving when it's first needed.

## Principles

- **Logs** are structured JSON on stdout (`log/slog`). Kubernetes collects stdout, so services never write log files.
- Every request carries a **request ID**, which is logged and passed on to downstream calls.
- **Never log secrets**, tokens or full account numbers.
- **Metrics** follow RED (Rate, Errors, Duration) per endpoint, plus business metrics such as postings and rejected transfers.
- **Traces** follow a request across services and through Kafka.

## Roadmap

| Phase | Adds |
|-------|------|
| 0 | JSON logs, `/healthz` + `/readyz` |
| 1 | Kubernetes liveness/readiness probes using those endpoints, logs via `kubectl logs` and the Tilt dashboard ✅ |
| 2 | Prometheus metrics (`/metrics`), kube-prometheus-stack, Grafana |
| 6 | Request ID in gRPC metadata, logging/metrics interceptors |
| 7 | Ledger audit trail and business metrics |
| 8 | OpenTelemetry tracing across services |
| 10–11 | Trace context in Kafka headers, outbox backlog and consumer lag metrics |
| 13 | Dashboards and alert rules |

## Current

`cmd/hello` writes JSON logs to stdout. Every line has `time`, `level`, `msg` and `service`. Every request produces one `http request` line:

```json
{"level":"INFO","msg":"http request","service":"hello","request_id":"demo-1","method":"GET","path":"/readyz","status":200,"duration_ms":0.04}
```

- **Request ID:** taken from the caller's `X-Request-Id` if it is 1–64 characters of `[A-Za-z0-9_-]`, otherwise generated. It is echoed back in the response header.
- **Log level:** `LOG_LEVEL` = `debug`, `info` (default), `warn` or `error`.
- **Health:** `/healthz` returns 200 while the process is alive. `/readyz` returns 200 while serving and 503 once shutdown begins.
- **Metrics:** `GET /metrics` serves Prometheus text format, built by `pkg/obs`:

  | Metric | Type | Labels | Answers |
  |---|---|---|---|
  | `http_server_request_duration_seconds` | histogram | `http_request_method`, `http_route`, `http_response_status_code` | All of RED: how many requests (`_count`), how many failed (status `5..`), how long they take (p50, p95, p99), including how long the failures took |
  | `http_server_active_requests` | gauge | none | How busy is it right now? |
  | `go_*`, `process_*` | various | none | Goroutines, memory, GC, CPU, open files |

  One histogram covers rate, errors and duration, because every histogram already counts its observations. A separate request counter would double the series for no extra answers. Metric and label names follow the [OpenTelemetry HTTP semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/), so dashboards and queries keep working when OpenTelemetry arrives in Phase 8.

  `http_route` is the matched route pattern (`/accounts/{id}`), never the raw path, so the number of time series stays small. Requests that match no route are labelled `unmatched`.

  Example PromQL queries, once Prometheus is running later in Phase 2:

  ```promql
  sum by (http_route) (rate(http_server_request_duration_seconds_count[5m]))            # rate
  sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
    / sum(rate(http_server_request_duration_seconds_count[5m]))                         # error ratio
  histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket[5m])))  # p95 latency
  ```
