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

Nothing emitted yet. Logging and health endpoints arrive in Phase 0 step 3.
