# Phase 1 — Docker, kind, Helm and Tilt

## What we built

- **One image recipe** (`Dockerfile`): a multi-stage build that compiles any service (`PKG=./cmd/<name>`) and ships only the binary in `distroless/static`, as UID 65532. `hello` is 15 MB. `make docker-smoke` checks that the image is healthy, non-root, and exits 0 on SIGTERM.
- **A local cluster** (`deploy/kind/cluster.yaml`): `make cluster-up` creates a one-node kind cluster in about 20 seconds.
- **One Helm chart** (`deploy/charts/service`) for every service, plus `deploy/values/hello.yaml`. The chart provides a Deployment (probes, resources, security context, graceful shutdown), a Service and a ConfigMap.
- **`make deploy`**: builds, tags the image by content (`build-<hash>`), loads it into kind and installs it with Helm.
- **The dev loop** (`Tiltfile`): `make dev` builds, deploys, port-forwards `localhost:8080`, and redeploys about 6 seconds after you save.
- **CI** now also builds and smoke-tests the image (`docker` job), and lints and schema-checks the chart for every service (`helm` job).
- **Decisions and rules:** [ADR-0002](../adr/0002-run-everything-in-docker-on-kubernetes.md) and [deploy/AGENTS.md](../../deploy/AGENTS.md).

## Concepts

- **Images and layers.** Each Dockerfile step is a cached layer. Copying `go.mod` before the source means dependencies download again only when they change.
- **Multi-stage builds** keep the toolchain out of the final image: less to attack, less to patch, faster to ship.
- **Cross-compiling.** `--platform=$BUILDPLATFORM` with `GOOS`/`GOARCH` builds natively on an arm64 Mac for any target.
- **Signals in containers.** The app is PID 1 and handles SIGTERM itself, so `docker stop` and Kubernetes get a clean exit, not a SIGKILL.
- **kind.** Each Kubernetes node is a Docker container with its own image store, so images are loaded in (`kind load`), not pulled from a registry.
- **Pods, Deployments and Services.** A pod runs containers. A Deployment keeps the right number of pods running and rolls out new versions. A Service gives them a stable name and only routes to ready pods.
- **Liveness vs readiness.** Failing liveness restarts the container. Failing readiness only stops traffic. See the rules in `deploy/AGENTS.md`.
- **Graceful termination on Kubernetes:** preStop sleep (5s), so the Service stops routing here first; then SIGTERM, with the app draining for `SHUTDOWN_TIMEOUT` (10s); then SIGKILL at the 20s grace period. The chart computes the grace period so the three can't drift apart.
- **Requests vs limits.** Requests are what the scheduler reserves. Going over a memory limit kills the container. We set no CPU limit, because CPU limits throttle even on idle nodes.
- **`runAsNonRoot` needs a numeric UID.** Kubernetes can't verify a user *name* without looking inside the image, so it refuses to start the container. We found this on the first deploy and fixed it with `USER 65532:65532`.
- **Helm.** A chart is templates with blanks, and a values file fills them in. A release records each install or upgrade as a revision, so you can roll back and uninstall cleanly.
- **kubeconform** validates rendered manifests against the Kubernetes API schemas without needing a cluster, so typos fail in CI.
- **Content-addressed tags.** A fixed tag (`:dev`) looks unchanged to Kubernetes after a rebuild. Tagging by a hash of the image's layers means unchanged code doesn't restart and changed code always rolls out. The image ID doesn't work for this, because it includes build timestamps.
- **Tilt** watches files, rebuilds only what changed, deploys with the same chart, and shows every service on one dashboard. The Tiltfile refuses any context but `kind-tiny-bank`.

## How to verify

```bash
make check
```

```bash
make docker-smoke
```

```bash
make chart-check
```

`make chart-check` needs `brew install kubeconform`.

```bash
make dev
```

In another terminal:

```bash
curl -i localhost:8080/readyz
```

Edit the message in `cmd/hello/server.go` and save. Within about 6 seconds, `curl localhost:8080/` returns the new text. Press `space` in the Tilt terminal for the dashboard. When you're done:

```bash
make dev-down
```

```bash
make cluster-down
```
