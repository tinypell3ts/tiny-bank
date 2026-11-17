# tiny-bank

A small core banking platform built from **Go microservices, Postgres, Kafka and Kubernetes**.

> **Status:** Phase 8a, done. Four services on a local Kubernetes cluster with a one-command dev loop: `hello`; the **account service**, which opens and looks up accounts; the **ledger**, which records every movement of money as double-entry journals, with balances that can't race or go negative, idempotency keys that give each request one final answer, and an append-only audit trail; and the **transaction service**, which saves each request to move money as a transfer and sees it through to the ledger, retrying in the background when the ledger is slow or down. The ledger checks each account with the account service. Every service is built on a shared foundation (`pkg/kit`, `pkg/db`) on Typhon and terrors, which gives them the same startup, request IDs, logs, metrics and safe errors, checked by one conformance suite. Their APIs are protobuf contracts that CI checks for breaking changes. Prometheus and Grafana run on the cluster, with a dashboard for the ledger.

## What it will be

Customers open accounts, deposit money and transfer it to each other. Every movement of money is recorded in a double-entry ledger, and customers see a feed of what happened.

The work is split across small services (account, ledger, transaction, notification and an edge gateway) that call each other over HTTP using [Typhon](https://github.com/monzo/typhon), with protobuf contracts. Each service has its own Postgres database, and events flow through Kafka. Everything runs in Docker on a local Kubernetes cluster.

See [docs/architecture.md](docs/architecture.md) for the full picture.

## Quickstart

Prerequisites:

- Go 1.27+
- [golangci-lint](https://golangci-lint.run) v2 (`brew install golangci-lint`)
- [buf](https://buf.build/docs/cli/installation/) for the protobuf contracts (`brew install bufbuild/buf/buf`)
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

Tilt builds the images, deploys them with Helm and forwards `hello` to `localhost:8080`, `account` to `localhost:8081`, `ledger` to `localhost:8082` and `transaction` to `localhost:8083`. It also installs Prometheus and Grafana, and the CloudNativePG operator that will run each service's Postgres database: Grafana is at [localhost:3000](http://localhost:3000) (no login locally, see the *tiny-bank / Services (RED)* dashboard) and Prometheus at [localhost:9090](http://localhost:9090). The first install takes a few minutes. It rebuilds and redeploys whenever you save a Go file. Press `space` to open the dashboard (localhost:10350), Ctrl-C to stop watching, `make dev-down` to remove what Tilt deployed (**this deletes each service's database and everything in it**, such as accounts; monitoring and CloudNativePG stay; `make monitoring-down` and `make cnpg-down` remove them), and `make cluster-down` to delete the cluster.

Open an account and look it up (the account service has its own Postgres, created on the first `make dev`):

```bash
curl -i -X POST localhost:8081/accounts -H 'Content-Type: application/json' \
  -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada Lovelace","currency":"GBP"}'
```

The response's `Location` header is the account's address, for example `curl localhost:8081/accounts/<id>`. The API is internal for now: it has no sign-in until the edge gateway in Phase 12.

Move £10 into an account from outside the bank, through the GBP funding account, then read its balance (amounts are in pence, and come back as strings):

```bash
curl -i -X POST localhost:8082/journals -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"deposit-1","currency":"GBP","entries":[{"account_id":"00000000-0000-4000-8000-000000000826","amount":-1000},{"account_id":"<account id>","amount":1000}]}'
curl localhost:8082/accounts/<account id>/balance
```

Send the same request again and you get the same journal back with a 200: the idempotency key makes a retry safe. The ledger checks each account with the account service first, so use the ID of an account you opened above.

Callers usually go through the transaction service, which saves each request as a transfer and sees it through to the ledger, retrying if the ledger is slow or down. Move £3 from that account to a second one you've opened:

```bash
curl -i -X POST localhost:8083/transfers -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"t1","from_account_id":"<account id>","to_account_id":"<other account id>","currency":"GBP","amount":300}'
```

A 201 means it finished in the request (`state` completed, or failed with a `failure_reason`), and a 202 that it's still pending: `GET localhost:8083/transfers/<id>` shows when it's done. `POST /deposits` takes an `account_id` and moves money in from the funding account.

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
- [Security](docs/security.md): what we protect, the controls and the known gaps ([how to report a vulnerability](SECURITY.md))
- [Decisions (ADRs)](docs/adr/): why we chose what we chose
- [Phases](docs/phases/): what each phase built and why
- [AGENTS.md](AGENTS.md): conventions and how we work (for humans and AI agents)
