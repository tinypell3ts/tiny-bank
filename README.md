# tiny-bank

A small core banking platform built from **Go microservices, Postgres, Kafka and Kubernetes**.

> **Status:** Phase 0, foundations. One demo service, `cmd/hello`.

## What it will be

Customers open accounts, deposit money and transfer it to each other. Every movement of money is recorded in a double-entry ledger, and customers see a feed of what happened.

The work is split across small services (account, ledger, transaction, notification and an edge gateway) that talk over gRPC. Each service has its own Postgres database, and events flow through Kafka. Everything runs in Docker on a local Kubernetes cluster.

See [docs/architecture.md](docs/architecture.md) for the full picture.

## Quickstart

Prerequisites:

- Go 1.27+
- [golangci-lint](https://golangci-lint.run) v2 (`brew install golangci-lint`)
- Docker Desktop (give it 8–12 GB RAM once the cluster arrives in Phase 1)

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

Or run it on a local Kubernetes cluster (needs `brew install kind kubectl helm`):

```bash
make cluster-up     # create the "tiny-bank" kind cluster
make deploy         # build the image, load it, and install it with Helm
kubectl --context kind-tiny-bank port-forward svc/hello 8080:80
```

A one-command dev loop with Tilt is coming later in Phase 1.

## Docs

- [Architecture](docs/architecture.md): how the pieces fit together
- [Observability](docs/observability.md): logs, metrics and traces
- [Decisions (ADRs)](docs/adr/): why we chose what we chose
- [Phases](docs/phases/): what each phase built and why
- [AGENTS.md](AGENTS.md): conventions and how we work (for humans and AI agents)
