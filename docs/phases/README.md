# Phases

tiny-bank is built in small phases, each one PR. Each phase gets its own page (`NN-title.md`) once it's built, covering what was built, the key concepts behind it and how to verify it.

Infrastructure comes first, so every service is born on Kubernetes.

| # | Phase | Page |
|---|-------|------|
| 0 | Go foundations + CI | [00-foundations](00-foundations.md) |
| 1 | Docker + kind + Helm + Tilt | [01-docker-kind-helm-tilt](01-docker-kind-helm-tilt.md) |
| 2 | Monitoring on cluster (Prometheus, Grafana) | [02-monitoring](02-monitoring.md) |
| 3 | `pkg/money` domain | [03-money](03-money.md) |
| 4 | Postgres via CloudNativePG + migrations | [04-postgres](04-postgres.md) |
| 5 | Account service (HTTP/JSON) on k8s, its `accounts` table and `/readyz` database check | [05-account-service](05-account-service.md) |
| 5.5 | Typhon-style filters in `pkg/kit` + `pkg/terrors` (the plain `pkg/kit` foundation arrived in Phase 5) | |
| 6 | Protobuf + gRPC | |
| 7 | Ledger service (double-entry) | |
| 8 | Transaction service + tracing | |
| 9 | Kafka via Strimzi | |
| 10 | Transactional outbox | |
| 11 | Notification service | |
| 11.5 | Customer service + onboarding: a customer exists (and is verified) before any account is opened for them | |
| 12 | Edge gateway + ingress: sign-in, and the customer ID comes from the signed-in caller, never the request body | |
| 13 | Hardening (limits, NetworkPolicies, alerts) | |
| 14 | CI end-to-end on kind | |
