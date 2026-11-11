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
| 1 | Kubernetes liveness/readiness probes using those endpoints |
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
