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
| 5.5 | Shared Typhon filters in `pkg/kit` for request IDs, access logs and RED metrics, applied to every service ✅ |
| 7 | Request ID passed on to downstream services by `kit.Get`, so one ID follows a request across services, and stored on each journal; ledger business metrics and dashboard ✅ |
| 8 | Jaeger on the cluster; OpenTelemetry tracing across services, database queries and the transaction finisher; trace and request IDs saved on each transfer ✅ |
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

- **Business metrics:** a service registers metrics about its own work on `db.Deps.Metrics`, and they're served at the same `/metrics`. The ledger's ([metrics.go](../services/ledger/metrics.go)):

  | Metric | Type | Labels | Answers |
  |---|---|---|---|
  | `ledger_journals_total` | counter | `currency`, `outcome` (`posted`, `retried`, `refused`, `failed`), `reason` (a refusal's terror code) | How much money moving is happening, how often retries arrive, and why posts are refused |
  | `ledger_moved_minor_units_total` | counter | `currency` | How much money new journals move, in minor units (pence for GBP) |

  Every label has a fixed set of values, never an account ID or an amount. Every refusal is counted once, including those the HTTP handler turns away before the ledger's rules run (invalid JSON, an unsupported currency, a malformed account ID), with currency `unknown` when there isn't a supported one yet. `reason` is a refusal's terror code, which is always one of the ledger's or `kit.Decode`'s own constants: none is built from what a caller sent, and the account service's codes are turned into the ledger's before they could reach a label.

  The transaction service's ([metrics.go](../services/transaction/metrics.go)):

  | Metric | Type | Labels | Answers |
  |---|---|---|---|
  | `transaction_ledger_posts_total` | counter | `by` (`request`, `finisher`), `outcome` (`posted`, `refused`, `key_reused`, `unsure`), `reason` (a refusal's terror code) | How often the ledger answers, refuses or leaves us unsure, and how much is left to the finisher |
  | `transaction_pending_transfers` | gauge | none | How many transfers are waiting for the ledger |
  | `transaction_oldest_pending_seconds` | gauge | none | How long the oldest has waited: the sign that transfers are stuck |

  The finisher sets both gauges each second, on every pod, so read them with `max()`. `reason` is one of the ledger's own fixed codes.

- **Following one request across services:** `kit.Get` and `kit.Post` pass the request ID and the trace on, so the same `request_id` is in the ledger's log lines and in the account service's lines for its checks. Each journal stores the request ID that posted it, so the audit trail leads from a movement of money to the logs of the request that made it.

- **Tracing:** `kit.Main` sets up OpenTelemetry for every service ([tracing.go](../pkg/obs/tracing.go), [ADR-0011](adr/0011-trace-requests-with-opentelemetry-and-jaeger.md)). With `OTEL_EXPORTER_OTLP_ENDPOINT` set (the chart sets `http://jaeger.jaeger:4318`), spans are batched and sent over OTLP/HTTP in the background, and trace context travels between services as the W3C `traceparent` header. Without it, tracing is off. Export failures are logged as warnings and never affect a request.

  `kit.Handler` gives every request a server span ([tracing.go](../pkg/kit/tracing.go)), continuing the caller's trace or starting one. It's named and labelled like the metrics: `POST /transfers`, with `http.request.method`, `http.route`, `http.response.status_code`, and `error.type` (the terror code) on an error. Only a 5xx marks a span failed. Probes get no span. Every line logged during a request carries its `trace_id` (`kit.NewLogger` adds it from the context to anything logged with `…Context(ctx, …)`), so a trace ID finds all of a request's lines in one grep, the full error from a 500 included, and any line leads to its trace in Jaeger. The access line also carries `request_id`. `kit.Get` and `kit.Post` give each call a client span, named by method and the other service (`POST ledger`), never the path, and send the trace on as `traceparent`, so one request is one trace across every service it touches: a deposit shows as `POST /deposits` → `POST ledger` → `POST /journals` → `GET account` → `GET /accounts/:id`. A call's span records the status only of a response that really came back, not of a connection that failed. Every query a request makes is a span too ([tracing.go](../pkg/db/tracing.go)), named by its operation and table (`UPDATE transfers`, `SELECT accounts FOR UPDATE`, `BEGIN`), and recording its SQL text but never the values passed for its `$1` placeholders. A batch is one span named by what's in it, with an event per query. A ledger post shows `BEGIN`, `SELECT pg_advisory_xact_lock` (the lock on its idempotency key: long means another post with the same key is deciding), `BATCH INSERT accounts, SELECT accounts FOR UPDATE` (the account locks: long means a row-lock wait on a busy account), `BATCH UPDATE accounts, INSERT entries` (the writes) and `COMMIT`. A failed query records its SQLSTATE as `error.type` (`P0001` for one of our triggers); in a batch, the failing query's event does too, unless the batch couldn't even be prepared. Queries outside any request (the finisher's polling, readiness checks, migrations) get no span. Each of the finisher's attempts is a trace of its own, `finish transfer`, holding `transfer.id`, `transfer.state` after the attempt, and `transfer.created_trace_id`, the trace of the request that created the transfer.

  Kafka messages carry the trace too ([kafka.go](../pkg/kafka/kafka.go)): `Producer.Produce` makes a producer span, `send <topic>`, with `messaging.destination.name` and `messaging.destination.partition.id`, and sends it on as a `traceparent` header in the message, so whatever handles it can continue the trace. No service sends messages yet.

### Finding a customer's transfer

A customer doesn't know a trace ID: they say "my £50 payment failed this morning". Start from the transfer. Each one keeps the trace ID and request ID of the request that created it, saved before the ledger is called, so even a transfer whose every ledger call timed out leads to its trace:

```bash
kubectl --context kind-tiny-bank exec -it transaction-db-1 -c postgres -- psql app -c \
  "SELECT id, state, failure_reason, amount, created_at, trace_id, request_id FROM transfers
   WHERE from_account = '<their account ID>' AND created_at > now() - interval '1 day' ORDER BY created_at DESC"
```

`state` and `failure_reason` usually answer the customer already (`precondition_failed.insufficient_funds`). When they don't (a transfer still pending, or a 500):

- paste `trace_id` into Jaeger's *Lookup by Trace ID* for the whole journey, and which hop failed;
- `kubectl --context kind-tiny-bank logs deploy/transaction | grep <request_id>` for the full error text, which spans leave out.

The saved trace is the creating request's. Each attempt the background finisher makes later is a trace of its own: search Jaeger for service `transaction` with the attribute `transfer.id=<id>` to see them all. Transfers saved before the IDs were kept have both empty. With tracing off, a new transfer still gets its `request_id` (`kit.Handler` gives every request one), so the log lookup still works; only `trace_id` is empty.

### On the cluster

`make dev` (or `make monitoring` on its own) installs [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) into the `monitoring` namespace, with settings in [deploy/monitoring/values.yaml](../deploy/monitoring/values.yaml):

| Part | What it does | Local URL |
|---|---|---|
| Prometheus Operator | Watches `ServiceMonitor` objects and turns them into Prometheus scrape config | |
| Prometheus | Scrapes `/metrics` every 30s and stores 1 day of data | [localhost:9090](http://localhost:9090) |
| Grafana | Charts. Comes with ~20 Kubernetes dashboards. No login locally | [localhost:3000](http://localhost:3000) |
| kube-state-metrics | Kubernetes object state: pod restarts, replicas, resource requests | |
| Jaeger | Receives traces over OTLP (`http://jaeger.jaeger:4318` inside the cluster) and shows them. In memory, lost on restart. Installed by `make jaeger`, in namespace `jaeger`. No login locally | [localhost:16686](http://localhost:16686) |

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

[tiny-bank / Ledger](http://localhost:3000/d/tiny-bank-ledger) shows what happens to money: journals posted per second, the share refused (the caller's fault) and failed (ours), posts by outcome, refusals by reason, and money moved by currency: in each interval, and in total for the time range. "Money moved" is volume (each journal's positive entries), because the net is always zero. Every series the ledger can name in advance (money moved, and posts posted, retried or failed, for every currency) starts at 0, so Prometheus sees the first journal as an increase. Refusal series appear on each reason's first refusal, so the dashboard misses that first one.

[tiny-bank / Transactions](http://localhost:3000/d/tiny-bank-transaction) shows whether transfers get finished: how many are pending and the oldest one's age (amber after a minute, red after five), ledger posts by outcome and by who tried, and refusals by reason.

The dashboards are JSON files in git ([services](../deploy/monitoring/dashboards/tiny-bank-services.json), [ledger](../deploy/monitoring/dashboards/tiny-bank-ledger.json), [transactions](../deploy/monitoring/dashboards/tiny-bank-transaction.json)). `make dashboards` puts it in a ConfigMap labelled `grafana_dashboard=1`, and Grafana's sidecar container loads it within seconds. Under `make dev`, Tilt does this every time the file is saved.

