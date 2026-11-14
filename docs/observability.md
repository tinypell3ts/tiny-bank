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
| 2 | Prometheus metrics (`/metrics`), kube-prometheus-stack, Grafana, a starter RED dashboard ✅ |
| 5.5 | Shared Typhon filters in `pkg/kit` for request IDs, access logs and RED metrics, applied to every service |
| 6 | Request ID passed on to downstream services in Typhon client calls, so one ID follows a request across services |
| 7 | Ledger audit trail and business metrics |
| 8 | OpenTelemetry tracing across services |
| 10–11 | Trace context in Kafka headers, outbox backlog and consumer lag metrics |
| 13 | Dashboards and alert rules |

## Current

Why metrics work this way, and what is deferred: [ADR-0003](adr/0003-measure-services-with-prometheus-red-metrics.md).

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

  `http_route` is the Typhon route pattern that matched (`/accounts/:id`), never the raw path, so the number of time series stays small. Requests that match no route are labelled `unmatched`. Methods outside the standard set (`GET`, `POST`, …) are labelled `_OTHER`, so a caller can't create new series by inventing methods. The status code is the one the caller received, including the 500 for a handler that panicked (still labelled with its route) and 499 for a caller that hung up. `kit.Handler` records all of this, so services never touch metrics for HTTP.

  Example PromQL queries, for Prometheus at localhost:9090 (add `job="hello"` to pick one service):

  ```promql
  sum by (http_route) (rate(http_server_request_duration_seconds_count[5m]))            # rate
  sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
    / sum(rate(http_server_request_duration_seconds_count[5m]))                         # error ratio
  histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket[5m])))  # p95 latency
  ```

### On the cluster

`make dev` (or `make monitoring` on its own) installs [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) into the `monitoring` namespace, with settings in [deploy/monitoring/values.yaml](../deploy/monitoring/values.yaml):

| Part | What it does | Local URL |
|---|---|---|
| Prometheus Operator | Watches `ServiceMonitor` objects and turns them into Prometheus scrape config | |
| Prometheus | Scrapes `/metrics` every 30s and stores 1 day of data | [localhost:9090](http://localhost:9090) |
| Grafana | Charts. Comes with ~20 Kubernetes dashboards. No login locally | [localhost:3000](http://localhost:3000) |
| kube-state-metrics | Kubernetes object state: pod restarts, replicas, resource requests | |

Turned off for a one-node dev cluster:
- Alertmanager, until alert rules arrive in Phase 13.
- node-exporter, because host metrics for the Docker VM aren't useful.
- Scraping etcd, the scheduler, the controller manager and kube-proxy, which kind doesn't expose.

Prometheus picks up every `ServiceMonitor` in the cluster, not only ones from this Helm release, so services can bring their own. The shared chart creates one per service ([servicemonitor.yaml](../deploy/charts/service/templates/servicemonitor.yaml)), so every service is scraped on its `http` port at `/metrics`. Each series gets `job` (the service name), `namespace` and `pod` labels. Check it in Prometheus under *Status → Targets*.

### Postgres

Each service database (`database.enabled` in the chart) comes with a `PodMonitor`, so Prometheus scrapes its Postgres metrics: connections, transactions, replication lag, database size and more (`cnpg_*` series). The CloudNativePG operator adds its own Grafana dashboard, *CloudNativePG*, which shows them per cluster. The operator itself is scraped too.

### Dashboards

[tiny-bank / Services (RED)](http://localhost:3000/d/tiny-bank-services) shows every service's real traffic, with a picker to choose services:

- Requests per second, 5xx error ratio and p95 latency at a glance.
- Requests per second by route and by status code.
- Latency by route at p50, p95 and p99.

It leaves out Kubernetes probes (`/healthz`, `/readyz`) and Prometheus scrapes (`/metrics`). Kubernetes calls the probes every few seconds, so they would otherwise swamp real traffic and make latency look better than it is. 4xx responses count as traffic but not as errors, because they are the caller's mistake.

The dashboard is [a JSON file in git](../deploy/monitoring/dashboards/tiny-bank-services.json). `make dashboards` puts it in a ConfigMap labelled `grafana_dashboard=1`, and Grafana's sidecar container loads it within seconds. Under `make dev`, Tilt does this every time the file is saved.

