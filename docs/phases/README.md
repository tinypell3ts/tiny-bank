# Phases

tiny-bank is built in small phases, each a stack of small PRs of about 200 changed lines (at most 400) ([AGENTS.md](../../AGENTS.md#working-agreement)). Each phase gets its own page (`NN-title.md`) once it's built, covering what was built, the key concepts behind it and how to verify it.

Infrastructure comes first, so every service is born on Kubernetes.

| # | Phase | Page |
|---|-------|------|
| 0 | Go foundations + CI | [00-foundations](00-foundations.md) |
| 1 | Docker + kind + Helm + Tilt | [01-docker-kind-helm-tilt](01-docker-kind-helm-tilt.md) |
| 2 | Monitoring on cluster (Prometheus, Grafana) | [02-monitoring](02-monitoring.md) |
| 3 | `pkg/money` domain | [03-money](03-money.md) |
| 4 | Postgres via CloudNativePG + migrations | [04-postgres](04-postgres.md) |
| 5 | Account service (HTTP/JSON) on k8s, its `accounts` table and `/readyz` database check | [05-account-service](05-account-service.md) |
| 5.5 | `pkg/kit` on Typhon and terrors: services as `typhon.Service`, shared filters, `kittest` | [05-5-typhon-kit](05-5-typhon-kit.md) |
| 6 | Protobuf contracts (`proto/`, `buf` lint, breaking and drift checks), served as JSON | [06-protobuf-contracts](06-protobuf-contracts.md) |
| 7 | Ledger service (double-entry journals, row locks, idempotency keys, an append-only audit trail), and the first service-to-service call (`kit.Get`, binary protobuf) | [07-ledger](07-ledger.md) |
| 8 | Transaction service (transfers as a state machine, retries with the same key, a background finisher) + tracing | [08-transaction-service](08-transaction-service.md) |
| 9 | Kafka via Strimzi | |
| 10 | Transactional outbox | |
| 10.5 | Hot accounts: make one very busy account (the funding account, a big merchant) fast, measured with the load tool: credits without a check, a single writer per account through Kafka, sharded accounts | |
| 11 | Notification service | |
| 11.5 | Customer service + onboarding: a customer exists (and is verified) before any account is opened for them | |
| 12 | Edge gateway + ingress: sign-in, and the customer ID comes from the signed-in caller, never the request body | |
| 13 | Hardening (limits, NetworkPolicies, alerts) | |
| 14 | CI end-to-end on kind | |
