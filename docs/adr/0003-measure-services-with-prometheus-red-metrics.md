# 3. Measure services with Prometheus RED metrics, scraped on the cluster

- **Status:** Accepted
- **Date:** 2026-11-12

## Context

A bank has to notice problems such as failing or slow payments as they happen, not when a customer complains. Logs tell us what happened to one request, but not how many requests are failing, or how slow they are overall. We need numbers for every service, collected in one place, with charts.

Constraints:

- Every service should report the same things in the same way, without each one setting it up.
- Metrics must never contain customer data such as account numbers, and callers must not be able to make them grow without limit.
- It has to run on the local kind cluster today and look like what production would run.
- Tracing (OpenTelemetry) arrives in Phase 8, so names chosen now should still make sense then.

## Decision

**Services expose metrics; Prometheus pulls them.** Every HTTP service serves `GET /metrics` in Prometheus text format, built by [`pkg/obs`](../../pkg/obs/metrics.go). Prometheus scrapes it every 30 seconds.

**One histogram covers rate, errors and duration (RED).** `http_server_request_duration_seconds{http_request_method, http_route, http_response_status_code}` records every request:

- **Rate:** its `_count` series.
- **Errors:** `_count` filtered to `http_response_status_code=~"5.."`.
- **Duration:** its buckets, from 1 ms to 10 s.

A histogram already counts its observations, so a separate request counter would double the series and answer nothing new. Keeping the status code on the histogram also shows how long failures took. The gauge `http_server_active_requests` shows load right now. Go runtime and process metrics come from the registry.

**Names follow the [OpenTelemetry HTTP semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/).** Queries and dashboards then keep working if metrics move to the OpenTelemetry SDK later.

**Labels have a small, fixed set of values:**

- **Route:** `http_route` is the matched route pattern (`/accounts/{id}`), never the raw path. Requests that match no route are labelled `unmatched`.
- **Method:** methods outside the standard nine are labelled `_OTHER`, so a caller can't create new series by inventing methods.
- **Status code:** the final one the client received, ignoring informational `1xx` responses and ignored second `WriteHeader` calls.

**Each service owns its registry** (`obs.NewRegistry()`), not Prometheus' global one, so tests get a clean registry and no library registers metrics behind our back.

**The cluster runs [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)**, pinned in the Makefile and installed by `make monitoring` (and by `make dev`). It bundles the Prometheus Operator, Prometheus, Grafana and kube-state-metrics. For a one-node dev cluster we switch off Alertmanager, node-exporter and the control-plane scrapes kind doesn't expose. Grafana has no login locally.

**Services are found automatically.** The shared Helm chart creates a `ServiceMonitor` for every service, and Prometheus picks up every ServiceMonitor in the cluster. `make chart-check` validates it against a pinned schema.

**Dashboards live in git as JSON.** Grafana's sidecar loads them from a labelled ConfigMap (`make dashboards`). The starter dashboard, *tiny-bank / Services (RED)*, leaves out probes and scrapes (`http_route!~"/healthz|/readyz|/metrics"`). Kubernetes calls the probes every few seconds, so they would otherwise swamp real traffic and make latency look better than it is.

### Alternatives considered

- **Count and time requests from logs.** No new pieces, but slow and costly to query, and it breaks whenever a log line changes.
- **Push metrics** (StatsD, the Prometheus Pushgateway, or OTLP push to a collector). Each service would need the collector's address and a retry policy, and a service that dies quietly looks the same as one that's idle. With pull, a failed scrape *is* the alert (`up == 0`), and services only keep counters in memory.
- **The OpenTelemetry SDK for metrics now.** It's the likely long-term direction, but today it adds an SDK, a collector and exporter configuration before we have a second signal to share them with. Matching OTel's names now keeps that move cheap. We'll revisit it with tracing in Phase 8.
- **A counter plus a histogram.** The classic Prometheus pattern, but it doubles the series, and the histogram without a status label can't show how long failures took.
- **Install Prometheus and Grafana by hand instead of kube-prometheus-stack.** Smaller, but we'd own the scrape config, RBAC, dashboards and upgrades. The stack is the standard on Kubernetes, and its Operator gives us ServiceMonitors.
- **Build dashboards in the Grafana UI.** Fast to start, but they'd be lost whenever the Grafana pod restarts, and they couldn't be reviewed in a PR.

### Deferred, on purpose

- **A separate metrics port.** `/metrics` shares the service's port for now. Before anything is exposed outside the cluster (the edge gateway, Phase 12), metrics move to their own port, or the gateway blocks `/metrics`, so the public internet can't read them.
- **Native histograms.** Our smallest bucket is 1 ms, so services answering in microseconds (like `hello`) all show latency just under 1 ms. Real services that call a database will sit well above that. Prometheus native histograms pick their own fine-grained buckets. We'll adopt them when sub-millisecond detail matters, or when we move to OpenTelemetry.
- **Exemplars**, which link a slow request on a latency chart to its trace. They need tracing first (Phase 8).
- **Alerts and Alertmanager**, in Phase 13.
- **Business metrics** (postings, rejected transfers), added with the ledger in Phase 7.

## Consequences

- A new HTTP service gets metrics, scraping and a place on the dashboard by using `pkg/obs` and the shared chart. It doesn't need any Prometheus or Grafana changes.
- Every request pays for one histogram observation, well under a microsecond.
- The cluster now needs the monitoring stack before services: Tilt waits for it, and `make deploy` stops with "Run: make monitoring" if it's missing. The first install takes a few minutes, and the stack uses about 0.8 GB of memory.
- The metric names are long, but they're the same ones OpenTelemetry tooling expects.
- Upgrading the stack is a deliberate change: bump `MONITORING_CHART_VERSION`, run `make monitoring`, and check the targets and dashboards.
- Prometheus keeps only one day of data locally. That's enough for development, but not a record of anything.
