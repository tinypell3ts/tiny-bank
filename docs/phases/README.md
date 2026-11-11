# Phases

tiny-bank is built in small phases, each one PR. Each phase gets its own page (`NN-title.md`) once it's built, covering what was built, the key concepts behind it and how to verify it.

Infrastructure comes first, so every service is born on Kubernetes.

| # | Phase | Page |
|---|-------|------|
| 0 | Go foundations + CI | [00-foundations](00-foundations.md) |
| 1 | Docker + kind + Helm + Tilt | [01-docker-kind-helm-tilt](01-docker-kind-helm-tilt.md) |
| 2 | Monitoring on cluster (Prometheus, Grafana) | |
| 3 | `pkg/money` domain | |
| 4 | Postgres via CloudNativePG + migrations | |
| 5 | Account service (HTTP/JSON) on k8s | |
| 5.5 | Extract `pkg/kit` + `pkg/terrors` (Typhon-style filters) | |
| 6 | Protobuf + gRPC | |
| 7 | Ledger service (double-entry) | |
| 8 | Transaction service + tracing | |
| 9 | Kafka via Strimzi | |
| 10 | Transactional outbox | |
| 11 | Notification service | |
| 12 | Edge gateway + ingress | |
| 13 | Hardening (limits, NetworkPolicies, alerts) | |
| 14 | CI end-to-end on kind | |
