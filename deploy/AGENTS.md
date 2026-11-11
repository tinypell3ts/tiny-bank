# deploy/AGENTS.md

Rules for everything under `deploy/`. The root [AGENTS.md](../AGENTS.md) still applies. Background and reasons are in [ADR-0002](../docs/adr/0002-run-everything-in-docker-on-kubernetes.md).

## Layout

```
deploy/
├── kind/cluster.yaml     # local cluster: one node, name set by CLUSTER in the Makefile
├── charts/service/       # THE chart, shared by every service
└── values/<service>.yaml # per-service settings, kept as small as possible
```

## Rules

- **One chart.** Never create a chart per service. If a service needs something the chart can't do, add it to `charts/service` behind a value with a safe default, so every service can use it.
- **Values files stay small.** A service's values file says what's different about that service, usually just `name` and `image.repository`. Settings every service should get belong in the chart's `values.yaml`.
- **Only target the local cluster.** Every command passes `--kube-context kind-tiny-bank` (or `--context`). Never rely on kubectl's current context. The `Tiltfile` refuses to run anywhere else.
- **Never deploy a fixed tag.** Deploy content-tagged images (`make deploy` → `build-<hash>`, Tilt → `tilt-<hash>`). A fixed tag like `:dev` looks unchanged to Kubernetes, so the old code keeps running.
- **Keep the security defaults.** Pods run as non-root with a numeric UID (`USER 65532:65532` in the Dockerfile), a read-only root filesystem, no privilege escalation, all capabilities dropped, and no service-account token. Loosen one only with an ADR explaining why.
- **Liveness never checks dependencies.** `/healthz` reports only "the process is alive". Database or Kafka checks belong in `/readyz`, otherwise a dependency blip restarts every pod at once.
- **Keep shutdown timings consistent.** `terminationGracePeriodSeconds` is computed as `preStopSleepSeconds + shutdownTimeoutSeconds + 5`. Change those two values, never the grace period directly. The app reads `SHUTDOWN_TIMEOUT` from the same value.
- **Memory limit, no CPU limit.** Set memory requests and limits, and CPU requests only. CPU limits throttle the app even on an idle node.
- **Don't mix `make dev` and `make deploy` for the same service.** Both create the same resources. Use `make dev-down` or `make undeploy` first.

## Checking changes

```bash
make chart-check   # helm lint + kubeconform for every values file (CI runs this too)
make dev           # then watch the Tilt dashboard (localhost:10350) go green
```

When you add a template or value, check it renders for every service: `make chart-check` loops over `deploy/values/*.yaml`.

## Adding a service

1. `cmd/<name>/main.go`, built on the same foundations as `cmd/hello`.
2. `deploy/values/<name>.yaml` with `name` and `image.repository: tiny-bank/<name>`.
3. One line in the `Tiltfile`: `go_service('<name>', port=<next free localhost port>)`.
4. `make chart-check`, then `make dev`.
