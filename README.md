# tiny-bank

A small core banking platform built from **Go microservices, Postgres, Kafka and Kubernetes**.

> **Status:** Phase 4, done. One demo service, `cmd/hello`, running in Docker on a local Kubernetes cluster with a one-command dev loop, Prometheus and Grafana, a shared `pkg/money` type for amounts, and Postgres for services: CloudNativePG on the cluster, `pkg/db` for connections and migrations.

## What it will be

Customers open accounts, deposit money and transfer it to each other. Every movement of money is recorded in a double-entry ledger, and customers see a feed of what happened.

The work is split across small services (account, ledger, transaction, notification and an edge gateway) that talk over gRPC. Each service has its own Postgres database, and events flow through Kafka. Everything runs in Docker on a local Kubernetes cluster.

See [docs/architecture.md](docs/architecture.md) for the full picture.

## Quickstart

Prerequisites:

- Go 1.27+
- [golangci-lint](https://golangci-lint.run) v2 (`brew install golangci-lint`)
- Docker Desktop (give it 8–12 GB RAM once the cluster arrives in Phase 1). `make test-integration` also uses it to start a throwaway Postgres.

```bash
make hooks   # once: blocks pushing badly named branches
make check   # format check, vet, lint, tests: the same as CI
```

Run the demo service:

```bash
go run ./cmd/hello                # PORT, LOG_LEVEL, SHUTDOWN_TIMEOUT are optional
curl localhost:8080/healthz
```

Or run it as a container:

```bash
make docker-build
docker run --rm -p 8080:8080 tiny-bank/hello:dev
```

Or run it on a local Kubernetes cluster, the everyday way (needs `brew install kind kubectl helm tilt`):

```bash
make dev            # creates the cluster if needed, then starts Tilt
curl localhost:8080/healthz
```

Tilt builds the image, deploys it with Helm and forwards `localhost:8080`. It also installs Prometheus and Grafana, and the CloudNativePG operator that will run each service's Postgres database: Grafana is at [localhost:3000](http://localhost:3000) (no login locally, see the *tiny-bank / Services (RED)* dashboard) and Prometheus at [localhost:9090](http://localhost:9090). The first install takes a few minutes. It rebuilds and redeploys whenever you save a Go file. Press `space` to open the dashboard (localhost:10350), Ctrl-C to stop watching, `make dev-down` to remove what Tilt deployed (monitoring and CloudNativePG stay; `make monitoring-down` and `make cnpg-down` remove them), and `make cluster-down` to delete the cluster.

Without Tilt, for a one-off deploy:

```bash
make cluster-up
make monitoring     # once: the chart's ServiceMonitor needs its CRDs
make cnpg           # once: the Postgres operator
make deploy
kubectl --context kind-tiny-bank port-forward svc/hello 8080:80
```

## Docs

- [Architecture](docs/architecture.md): how the pieces fit together
- [Observability](docs/observability.md): logs, metrics and traces
- [Decisions (ADRs)](docs/adr/): why we chose what we chose
- [Phases](docs/phases/): what each phase built and why
- [AGENTS.md](AGENTS.md): conventions and how we work (for humans and AI agents)
