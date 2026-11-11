# 2. Run everything in Docker on Kubernetes, locally with kind, Helm and Tilt

- **Status:** Accepted
- **Date:** 2026-11-11

## Context

tiny-bank will be several Go services (account, ledger, transaction, notification and an edge gateway), each with its own Postgres database, talking over gRPC and Kafka. Its target runtime is Kubernetes.

Much of what makes a service safe to run only shows up on Kubernetes: health and readiness probes, graceful shutdown on SIGTERM, resource limits, non-root security settings and rolling updates. If developers run services one way locally and another way in production, those behaviours go untested until late, and the two setups drift apart.

We needed a local setup that:

- is the same shape as production (containers on Kubernetes, deployed the same way),
- is quick enough to use for every change,
- is cheap to add services to,
- can't accidentally touch a real cluster.

## Decision

Every service runs in Docker on Kubernetes from the start, locally as well as later in production.

- **Images:** one multi-stage `Dockerfile` for all services, selected with `--build-arg PKG=./cmd/<name>`. The runtime stage is `distroless/static` running as UID 65532.
- **Local cluster:** [kind](https://kind.sigs.k8s.io) ("Kubernetes in Docker"), one node, created with `make cluster-up`.
- **Deployment:** one generic [Helm](https://helm.sh) chart, `deploy/charts/service`, plus a small values file per service in `deploy/values/`. The chart owns probes, resource limits, security context and shutdown timing, so services don't repeat them.
- **Dev loop:** [Tilt](https://tilt.dev). `make dev` builds, deploys with the same chart, port-forwards, and redeploys on save. `make deploy` remains for one-off deploys without Tilt.
- **Image tags:** deployed images are tagged by content (Tilt's `tilt-<hash>`, or `build-<hash>` from `make deploy`), never a fixed tag like `:dev`, so a code change always rolls out.
- **Safety:** the Tiltfile and Makefile always target the `kind-tiny-bank` context.

Docker Compose is used only by integration tests (testcontainers), never as a second way to run the stack.

### Alternatives considered

- **Run from source or Compose now, Kubernetes later.** Quicker to start, but Kubernetes behaviours go untested until late, and we'd maintain two setups.
- **minikube or k3d instead of kind.** All are viable. kind is the most common choice in CI, so the same cluster type can run our end-to-end tests later, and it needs nothing beyond Docker.
- **Raw manifests or Kustomize instead of Helm.** Raw manifests would be copied per service. Kustomize patches work, but Helm's templates and values suit "one chart, many services" more simply. Helm also gives install, upgrade and rollback history, and the operators we'll use later (CloudNativePG, Strimzi) ship as Helm charts.
- **Skaffold instead of Tilt.** A close alternative. Tilt has a better multi-service dashboard, and its Starlark config lets us declare each service in one line (`go_service(...)`).

## Consequences

- Developers need Docker Desktop (8–12 GB RAM once databases and Kafka arrive), kind, kubectl, Helm and Tilt.
- A new service needs a `cmd/<name>`, a values file and one `Tiltfile` line. It inherits probes, limits, security and shutdown behaviour from the chart.
- Kubernetes problems surface locally and in CI. For example, `runAsNonRoot` rejecting a non-numeric `USER` was caught on the first deploy, not in production.
- A rebuild-and-redeploy takes about 6 seconds, slower than `go run`. If that becomes painful, Tilt's live update can sync files into running pods.
- The chart is shared, so a change to it affects every service. CI runs `helm lint` and kubeconform against every values file to catch mistakes.
