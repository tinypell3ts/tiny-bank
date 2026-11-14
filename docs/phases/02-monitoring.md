# Phase 2 — Monitoring on the cluster

## What we built

- **Shared metrics** (`pkg/obs`): a registry with Go runtime and process metrics, a `/metrics` handler, and HTTP middleware that records one histogram, `http_server_request_duration_seconds`. Its labels are method, route and status code, so it covers rate, errors and duration. It also records a gauge, `http_server_active_requests`. `hello` serves them at `GET /metrics`.
- **A safe status recorder** (`obs.StatusRecorder`): it records the status code the client actually received. Both the metrics and `hello`'s access log use it. (Phase 5.5 replaced the middleware and recorder with Typhon filters in `pkg/kit`, which read the status from the response.)
- **The monitoring stack** (`deploy/monitoring/values.yaml`): `make monitoring` installs kube-prometheus-stack (Prometheus Operator, Prometheus, Grafana, kube-state-metrics) into the `monitoring` namespace. `make dev` does too, and forwards Grafana to `localhost:3000` and Prometheus to `localhost:9090`.
- **Automatic scraping:** the shared chart creates a `ServiceMonitor` for each service, so Prometheus finds and scrapes every service. `make chart-check` validates it against a pinned schema.
- **A starter dashboard** (`deploy/monitoring/dashboards/`): *tiny-bank / Services (RED)* shows requests per second, 5xx error ratio and latency per service and route, without probes and scrapes. It's JSON in git, loaded by `make dashboards`. Under Tilt it reloads on save, and CI checks it with `make dashboards-check`.
- **Decisions and rules:** [ADR-0003](../adr/0003-measure-services-with-prometheus-red-metrics.md), [observability.md](../observability.md) and [deploy/AGENTS.md](../../deploy/AGENTS.md).

## Concepts

- **Pull vs push.** Services keep counters in memory and Prometheus fetches them every 30 seconds. A failed fetch is a signal in itself (`up == 0`), and services don't need to know where Prometheus is.
- **Counter, gauge, histogram.**
  - A counter only goes up, like total requests.
  - A gauge goes up and down, like requests in progress.
  - A histogram counts observations into buckets (≤1 ms, ≤2.5 ms, …), so Prometheus can estimate percentiles such as p95.
- **RED:** Rate, Errors and Duration, the three questions to ask of any request-driven service. One histogram with a status-code label answers all three.
- **Cardinality.** Every distinct set of label values is a separate time series held in memory. Labels must have a small, fixed set of values. That's why we use the route pattern `/accounts/{id}`, never the raw path, and turn unknown methods into `_OTHER`.
- **`r.Pattern`.** Go's `ServeMux` (Go 1.22+) records the pattern that matched on the request. Middleware reads it *after* calling the mux, which is how the metrics get the route.
- **Wrapping `http.ResponseWriter`.** To find out the status code, middleware wraps the writer. Informational `1xx` responses aren't final, a second `WriteHeader` is ignored by Go, and a body written without `WriteHeader` means 200. `Unwrap` lets `http.ResponseController` reach the real writer for features like flushing.
- **Data races.** Two goroutines touching the same variable need a channel, a lock or an atomic between them. Getting a network response back isn't enough. `go test -race` catches this.
- **Operators and CRDs.** A CRD adds a new object type to Kubernetes, such as `ServiceMonitor`. An operator is a program that watches those objects and acts on them. Here, the Prometheus Operator turns each ServiceMonitor into scrape config.
- **Grafana's sidecar.** A helper container next to Grafana watches for ConfigMaps labelled `grafana_dashboard=1` and loads the dashboards in them. That keeps dashboards in git.
- **Percentiles from buckets.** `histogram_quantile` estimates inside a bucket, so it can't be more precise than the buckets. Everything faster than 1 ms shows as just under 1 ms.

## How to verify

```bash
make check
```

```bash
make chart-check
```

```bash
make dashboards-check
```

```bash
make dev
```

The first run installs the monitoring stack, which takes a few minutes. Then, in another terminal:

```bash
curl -s localhost:8080/metrics | grep ^http_server
```

Open [localhost:9090/targets](http://localhost:9090/targets): `hello` is listed and up. Send some traffic:

```bash
for i in $(seq 1 100); do curl -s -o /dev/null localhost:8080/; sleep 0.2; done
```

Open [localhost:3000/d/tiny-bank-services](http://localhost:3000/d/tiny-bank-services). Within a minute, requests per second rise for `hello GET /`, and the error ratio stays at 0%. When you're done:

```bash
make dev-down
```

```bash
make monitoring-down
```
