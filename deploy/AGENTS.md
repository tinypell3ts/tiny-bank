# deploy/AGENTS.md

Rules for everything under `deploy/`. The root [AGENTS.md](../AGENTS.md) still applies. Background and reasons are in [ADR-0002](../docs/adr/0002-run-everything-in-docker-on-kubernetes.md).

## Layout

```
deploy/
├── kind/cluster.yaml     # local cluster: one node, name set by CLUSTER in the Makefile
├── charts/service/       # THE chart, shared by every service
├── charts/service/ci/    # extra values chart-check renders, for features no service uses yet
├── cnpg/values.yaml       # CloudNativePG operator settings (runs Postgres for services)
├── monitoring/values.yaml # kube-prometheus-stack settings for the local cluster
├── monitoring/dashboards/ # Grafana dashboards as JSON, loaded by `make dashboards`
└── values/<service>.yaml # per-service settings, kept as small as possible
```

## Rules

- **One chart.** Never create a chart per service. If a service needs something the chart can't do, add it to `charts/service` behind a value with a safe default, so every service can use it.
- **Values files stay small.** A service's values file says what's different about that service, usually just `name` and `image.repository`. Settings every service should get belong in the chart's `values.yaml`.
- **Only target the local cluster.** Every command passes `--kube-context kind-tiny-bank` (or `--context`). Never rely on kubectl's current context. The `Tiltfile` refuses to run anywhere else.
- **Never deploy a fixed tag.** Deploy content-tagged images (`make deploy` → `build-<hash>`, Tilt → `tilt-<hash>`). A fixed tag like `:dev` looks unchanged to Kubernetes, so the old code keeps running.
- **Keep the security defaults.** Pods run as non-root with a numeric UID (`USER 65532:65532` in the Dockerfile), a read-only root filesystem, no privilege escalation, all capabilities dropped, and no service-account token. Loosen one only with an ADR explaining why.
- **Startup probe first.** Every pod has a startup probe on `/healthz` (up to 150s). Until it passes, Kubernetes runs no liveness or readiness checks, so a service that connects and migrates before listening isn't restarted mid-start. Keep a service's own startup timeout (2 minutes for `cmd/account`) below 150s.
- **Liveness never checks dependencies.** `/healthz` reports only "the process is alive". Database or Kafka checks belong in `/readyz`, otherwise a dependency blip restarts every pod at once.
- **Keep shutdown timings consistent.** `terminationGracePeriodSeconds` is computed as `preStopSleepSeconds + shutdownTimeoutSeconds + 5`. Change those two values, never the grace period directly. The app reads `SHUTDOWN_TIMEOUT` from the same value.
- **Memory limit, no CPU limit.** Set memory requests and limits, and CPU requests only. CPU limits throttle the app even on an idle node.
- **Third-party charts are pinned.** Their versions live in the Makefile (`MONITORING_CHART_VERSION`, `CNPG_CHART_VERSION`). Bump one on purpose, run its target (`make monitoring`, `make cnpg`), and check it works before committing: Grafana and Prometheus targets for monitoring, a database becoming ready for CloudNativePG.
- **Install order: monitoring, then CloudNativePG, then services.** Each one needs CRDs from the one before. Tilt enforces this order with `resource_deps`: `cnpg` waits for `monitoring`, and every `go_service` waits for both.
- **Never delete CloudNativePG's CRDs.** Deleting the `clusters.postgresql.cnpg.io` CRD deletes every database with it. The chart keeps its CRDs on `helm uninstall` (`make cnpg-down` only removes the operator), so leave it that way.
- **`deploy/monitoring/values.yaml` is local-only.** Grafana has no login and everyone is admin. Never reuse it for a shared cluster.
- **Service APIs are internal until Phase 12.** Services have no authentication yet: the account API lets any caller act for any customer. Keep their Kubernetes Services `ClusterIP` and add no Ingress or `LoadBalancer` until the edge gateway checks who the caller is.
- **Every service is scraped.** The chart creates a `ServiceMonitor` for each service, so Prometheus scrapes `GET /metrics` on the `http` port without extra setup. That needs the monitoring stack's CRDs, so the monitoring stack is installed first: Tilt waits for it, and `scripts/deploy.sh` stops with "Run: make monitoring" if it's missing.
- **Dashboards live in git.** Grafana dashboards are JSON files in `monitoring/dashboards/`, each with a fixed `uid`. Changes made in the Grafana UI are lost when its pod restarts, so build in the UI, then use *Export → Export as JSON* and save over the file. Panels about service traffic filter out probes and scrapes with `http_route!~"/healthz|/readyz|/metrics"`.
- **One database per service, declared in the chart.** A service that stores data sets `database.enabled: true` in its values file. The chart then creates a CloudNativePG `Cluster` called `<name>-db`, a `PodMonitor` so Prometheus scrapes Postgres, and a `DATABASE_URL` env var. That variable is read from the `<name>-db-app` secret the operator generates. Never hard-code database passwords, and never let two services share a database: services talk to each other through their APIs.
- **Custom resources are validated too.** `make chart-check` checks custom types like `ServiceMonitor` against schemas from the [CRDs catalog](https://github.com/datreeio/CRDs-catalog), pinned to one commit by `CRD_SCHEMAS` in the Makefile. A new custom type must have a schema there, otherwise the check fails.
- **Teardown deletes data.** A service's database (`<name>-db`) belongs to its release: its storage is owned by the CloudNativePG `Cluster`, so `make dev-down`, `make undeploy` and `make cluster-down` all delete the database and every row in it. Locally that's fine, but say so wherever these commands are suggested. Stop Tilt with Ctrl-C instead to keep the data.
- **Don't mix `make dev` and `make deploy` for the same service.** Both create the same resources. Use `make dev-down` or `make undeploy` first, which deletes the service's database.

## Checking changes

```bash
make chart-check   # helm lint + kubeconform for every values file (CI runs this too)
make dev           # then watch the Tilt dashboard (localhost:10350) go green
```

When you add a template or value, check it renders for every service: `make chart-check` loops over `deploy/values/*.yaml` and `charts/service/ci/*.yaml`. If no service turns a feature on yet, add a `ci/` values file that does, so its templates are still validated.

## Adding a service

1. `cmd/<name>/main.go`, built on the same foundations as `cmd/hello`.
2. `deploy/values/<name>.yaml` with `name` and `image.repository: tiny-bank/<name>`. Add `database: {enabled: true}` if it stores data.
3. One line in the `Tiltfile`: `go_service('<name>', port=<next free localhost port>)`. If its values file turns on `database.enabled`, `go_service` also adds a `<name>-db` row with the database's pods and logs. The Tilt dashboard groups each service's rows under its name, so the account group shows `account` and `account-db` together.
4. `make chart-check`, then `make dev`.
