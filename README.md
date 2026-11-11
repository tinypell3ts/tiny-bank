# tiny-bank

A small, Monzo-style core bank, built phase by phase to learn **Go microservices, Postgres, Kafka and Kubernetes**.

> **Status:** Phase 0, foundations. No runnable service yet.

## What it will be

Customers open accounts, deposit money and transfer it to each other. Every movement of money is recorded in a double-entry ledger, and customers see a feed of what happened.

The work is split across small services (account, ledger, transaction, notification and an edge gateway) that talk over gRPC. Each service has its own Postgres database, and events flow through Kafka. Everything runs in Docker on a local Kubernetes cluster.

See [docs/architecture.md](docs/architecture.md) for the full picture.

## Quickstart

Prerequisites:

- Go 1.27+
- Docker Desktop (give it 8–12 GB RAM once the cluster arrives in Phase 1)

```bash
go build ./...
go test ./...
```

Running the stack (kind, Helm, Tilt) arrives in Phase 1.

## Docs

- [Architecture](docs/architecture.md): how the pieces fit together
- [Observability](docs/observability.md): logs, metrics and traces
- [Decisions (ADRs)](docs/adr/): why we chose what we chose
- [Phases](docs/phases/): what each phase built and what it teaches
- [AGENTS.md](AGENTS.md): conventions and how we work (for humans and AI agents)
