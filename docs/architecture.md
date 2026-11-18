# Architecture

This page grows with the project. The **target** shows where we're heading. **Current** shows what actually exists today.

## Target

```mermaid
flowchart LR
    client([Client]) -->|REST via ingress| gw[edge-gateway]
    gw -->|Typhon| customer
    customer -->|Typhon: open first account| account
    gw -->|Typhon| account
    gw -->|Typhon| txn[transaction]
    txn -->|Typhon| ledger
    account & ledger & txn -->|outbox| kafka[(Kafka)]
    kafka --> notif[notification]
    customer --- cdb[(Postgres)]
    account --- adb[(Postgres)]
    ledger --- ldb[(Postgres)]
    txn --- tdb[(Postgres)]
    notif --- ndb[(Postgres)]
```

- **Services:** Go, built on [Typhon](https://github.com/monzo/typhon), an RPC framework that wraps `net/http`, with errors as [terrors](https://github.com/monzo/terrors). Each service's API is a protobuf contract in `proto/`, checked by `buf` for breaking changes, and services call each other over HTTP ([ADR-0007](adr/0007-build-services-on-typhon-and-terrors.md)).
- **Runtime:** Docker on Kubernetes (kind locally). Services are deployed with one generic Helm chart, and Tilt runs the dev loop.
- **Data:** one Postgres database per service, managed by CloudNativePG. Services never read another service's database.
- **Events:** services publish through a transactional outbox to Kafka (Strimzi). Consumers deduplicate with an inbox table.
- **Money:** `int64` minor units plus a currency code. Never floats.
- **Customers before accounts:** the customer service onboards a person (identity checks, terms) and creates their customer ID, then opens their first account. The account service never creates customers. Once someone is signed in, the edge gateway takes their customer ID from the sign-in token; customers never send it themselves ([ADR-0006](adr/0006-onboard-customers-before-accounts.md)).

## Current

Four services, `hello`, `account`, `ledger` and `transaction` (each of the last three with its own Postgres), plus the monitoring stack and the Postgres operator, running on a local kind cluster:

```mermaid
flowchart LR
    dev([Developer]) -->|make dev| tilt[Tilt]
    tilt -->|build| img[hello, account, ledger, transaction images<br/>distroless, UID 65532]
    tilt -->|helm chart: deploy/charts/service| k8s
    subgraph k8s [kind cluster: kind-tiny-bank]
        svc[Service hello] --> pod[Pod hello<br/>probes /healthz, /readyz<br/>/metrics]
        sm[ServiceMonitor hello] -.selects.-> svc
        subgraph mon [namespace: monitoring]
            prom[Prometheus] --> graf[Grafana]
        end
        prom -->|scrapes /metrics every 30s| pod
        asvc[Service account] --> apod[Pod account<br/>POST /accounts<br/>GET /accounts/id]
        apod -->|DATABASE_URL| adb[(account-db<br/>CloudNativePG)]
        prom -->|scrapes| apod
        prom -->|scrapes| adb
        lsvc[Service ledger] --> lpod[Pod ledger<br/>POST /journals<br/>GET /accounts/id/balance]
        lpod -->|DATABASE_URL| ldb[(ledger-db<br/>CloudNativePG)]
        prom -->|scrapes| lpod
        lpod -->|kit.Get /accounts/id<br/>binary protobuf| asvc
        tsvc[Service transaction] --> tpod[Pod transaction<br/>POST /transfers, /deposits<br/>GET /transfers/id<br/>finisher in background]
        tpod -->|DATABASE_URL| tdb[(transaction-db<br/>CloudNativePG)]
        prom -->|scrapes| tpod
        tpod -->|kit.Post /journals<br/>binary protobuf| lsvc
        subgraph cnpgns [namespace: cnpg-system]
            cnpg[CloudNativePG operator]
        end
        subgraph kns [namespace: kafka]
            strimzi[Strimzi operator]
        end
        subgraph jns [namespace: jaeger]
            jaeger[Jaeger v2<br/>OTLP in, traces in memory]
        end
    end
    tilt -->|make monitoring: kube-prometheus-stack| mon
    tilt -->|make cnpg| cnpgns
    tilt -->|make jaeger| jns
    tilt -->|make strimzi| kns
    dev -->|localhost:8080 port-forward| pod
    dev -->|localhost:8081 port-forward| apod
    dev -->|localhost:8082 port-forward| lpod
    dev -->|localhost:8083 port-forward| tpod
    cnpg -.runs.-> adb
    cnpg -.runs.-> ldb
    cnpg -.runs.-> tdb
    dev -->|localhost:3000 Grafana, :9090 Prometheus| mon
    dev -->|localhost:16686 Jaeger UI| jns
```

- **Image:** one shared `Dockerfile`, multi-stage, distroless and non-root.
- **Cluster:** one-node kind cluster (`make cluster-up`).
- **Deploy:** one shared Helm chart plus a small values file per service (`deploy/values/hello.yaml`, `account.yaml`, `ledger.yaml`, `transaction.yaml`). The chart also creates a `ServiceMonitor`, so Prometheus scrapes every service.
- **Dev loop:** Tilt rebuilds and redeploys on save.
- **Monitoring:** kube-prometheus-stack (Prometheus Operator, Prometheus, Grafana, kube-state-metrics) in the `monitoring` namespace, installed by `make monitoring`. See [observability.md](observability.md).
- **Kafka:** the [Strimzi](https://strimzi.io) operator in the `kafka` namespace, installed by `make strimzi`. It turns a `Kafka` object into running brokers, with their storage and config, as CloudNativePG does for Postgres. Strimzi 1.x runs Kafka in KRaft mode, with no ZooKeeper. No Kafka cluster runs yet.
- **Tracing:** [Jaeger](https://www.jaegertracing.io) v2 in the `jaeger` namespace, installed by `make jaeger`: all-in-one, receiving traces over OTLP and keeping them in memory. Services don't send it traces yet.
- **Postgres:** the [CloudNativePG](https://cloudnative-pg.io) operator in `cnpg-system`, installed by `make cnpg`. It turns each `Cluster` object into a running Postgres, with its users, passwords, storage and failover. A service turns on `database.enabled` in its values file, and the shared chart gives it its own Postgres `Cluster` (`<name>-db`), with its connection string passed in as `DATABASE_URL`.
- **Account service:** `cmd/account` plus `services/account`. It connects to `account-db` and applies its migrations before serving, and offers `POST /accounts` and `GET /accounts/:id` on Typhon, as JSON following its contract, `proto/account/v1`. It's internal only (ClusterIP, no sign-in) until the edge gateway.
- **Ledger service:** `cmd/ledger` plus `services/ledger`, with its own `ledger-db`. It records every movement of money as a double-entry journal whose entries add up to zero, locking the accounts it touches so balances can't race, and takes an idempotency key so a retried request moves money once. Before moving money it checks every customer account with the account service (`ACCOUNT_URL`, `http://account` on the cluster), the first call between services: binary protobuf through `kit.Get`, with the request ID passed on. It offers `POST /journals`, `GET /journals/:id` and `GET /accounts/:id/balance`, following `proto/ledger/v1`. Internal only, like the account service.
- **Transaction service:** `cmd/transaction` plus `services/transaction`, with its own `transaction-db`. It saves each request to move money as a transfer (pending, then completed or failed) before posting its journal to the ledger (`LEDGER_URL`) with `kit.Post`, keyed by the transfer's own ID, so retries move money once. A background finisher on every pod retries what requests left pending ([ADR-0010](adr/0010-move-money-as-transfers-saved-before-the-ledger-is-called.md)). It offers `POST /transfers`, `POST /deposits` and `GET /transfers/:id`, following `proto/transaction/v1`. Internal only.

See [ADR-0002](adr/0002-run-everything-in-docker-on-kubernetes.md) for why we run on Kubernetes, [ADR-0003](adr/0003-measure-services-with-prometheus-red-metrics.md) for how we measure services, and [deploy/AGENTS.md](../deploy/AGENTS.md) for the rules.
