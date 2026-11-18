# Security

What tiny-bank protects, where its trust boundaries are, which controls are in place and what each one stops, and the gaps that are still open. The rules every change must follow are in the root [AGENTS.md](../AGENTS.md#security), including keeping this page true: a change that touches a control, a gap or a file they link to updates its row in the same PR. To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## What we protect

| Asset | Why it matters |
|---|---|
| **Money** | A balance must never be wrong. Amounts are `int64` minor units, never floats ([ADR-0004](adr/0004-represent-money-as-int64-minor-units.md)); the ledger is the record of every movement, as double-entry journals ([ADR-0009](adr/0009-record-money-as-double-entry-journals-with-locked-balances.md)). |
| **Personal data** | Holder names and customer IDs identify real people. They stay in the owning service's database, and out of side channels such as metric labels, error messages and terror params. |
| **Secrets** | `DATABASE_URL` holds a database password. Leaking it gives full access to that service's data. |
| **Availability** | One bad request, or one misbehaving caller, mustn't take a service down for everyone. |

## Trust boundaries

```mermaid
flowchart LR
    outside([Anyone outside the cluster]) -. "Phase 12: edge gateway, sign-in" .-> gw[edge gateway]
    dev([Developer on localhost]) -->|kubectl port-forward| account
    gw -.-> account[account service]
    other[any pod in the cluster] -->|"no NetworkPolicy yet (Phase 13)"| account
    account -->|its own credentials| db[(account-db)]
```

- **Outside the cluster → a service:** nothing crosses this today. Every service is a `ClusterIP` Service with no Ingress. The only way in is `kubectl port-forward`, which listens on localhost. In Phase 12 the edge gateway becomes the one way in, and checks who the caller is.
- **Pod → service, inside the cluster:** trusted completely today. Any pod can call any service, and the account API believes whatever customer ID a caller sends ([ADR-0006](adr/0006-onboard-customers-before-accounts.md)). This is the biggest gap, and why nothing may be exposed before Phase 12.
- **Service → its database:** each service has its own Postgres and its own generated credentials, and only that service's pods get them. No service reads another's database.
- **Service → caller (errors):** callers, including other services, see a terror's code, message and params, and nothing else.

## Controls in place

| Control | Stops | Where |
|---|---|---|
| Services are `ClusterIP` only, with no Ingress | Anyone outside the cluster reaching an API that has no sign-in | [service.yaml](../deploy/charts/service/templates/service.yaml) |
| `kit.Decode` is strict: `Content-Type: application/json` or `application/protobuf`, one JSON object, and no unknown fields or enum values in either format | A browser posting cross-site as a "simple" request; typos silently ignored; surprising shapes such as `null` | [decode.go](../pkg/kit/decode.go) |
| Every body capped at 16 KB with `http.MaxBytesReader`, which also closes the connection | Tying up memory or bandwidth with a huge body | [handler.go](../pkg/kit/handler.go) |
| Caller input parsed into typed values at the edge; only canonical UUIDs accepted | Malformed values reaching storage; one account reachable under many URLs | [uuid.go](../pkg/kit/uuid.go) (`kit.ParseUUID`), each service's `http.go` |
| Kafka topics are declared in git (`KafkaTopic`), and auto-creating topics is off | A client creating topics by writing to a made-up or mistyped name | [topics.yaml](../deploy/kafka/topics.yaml), [kafka.yaml](../deploy/kafka/kafka.yaml) |
| Every query passes values as parameters | SQL injection | [account](../services/account/store.go), [ledger](../services/ledger/store.go) and [transaction](../services/transaction/store.go) `store.go` |
| Database `CHECK` constraints repeat the key rules, including "a customer balance is never negative" | Bad data, even if the code ever let it through | [account](../services/account/migrations/), [ledger](../services/ledger/migrations/) and [transaction](../services/transaction/migrations/) migrations |
| Error cleaning: only a terror's code, message and params leave a service | Stack traces, file paths and raw database errors reaching callers | [handler.go](../pkg/kit/handler.go), `cleanErrors` |
| Full errors logged with the request ID | Failures nobody can explain afterwards | [handler.go](../pkg/kit/handler.go) |
| `kit.Secret` prints as `xxxxx` in `fmt`, `slog`, errors and JSON | A secret leaking because someone logged a config | [secret.go](../pkg/kit/secret.go), [ADR-0008](adr/0008-stop-secrets-where-they-enter.md) |
| Caller request IDs reused only if short and plain (`A-Z a-z 0-9 - _`, at most 64) | Fake log lines injected through a header | [requestid.go](../pkg/kit/requestid.go) |
| Spans are named and labelled like metrics: route patterns (or, for a call to another service, its name), a fixed set of methods, status codes and terror codes, never a raw path or anything a caller typed; a query's span holds its SQL text, never the values passed for its placeholders; probes get no span | Personal data in traces; callers creating unlimited span names with made-up paths or methods | [tracing.go](../pkg/kit/tracing.go), [ADR-0011](adr/0011-trace-requests-with-opentelemetry-and-jaeger.md) |
| Metric labels use route patterns and a fixed set of methods | Account IDs in metrics, and callers creating unlimited time series | [handler.go](../pkg/kit/handler.go), [metrics.go](../pkg/obs/metrics.go) |
| A journal's entries must add up to zero; customer balances can't go below zero; each entry is capped at 10^12 minor units | Money created or destroyed by a bad request; overdrafts; a few huge journals pushing a balance to the int64 limit, so that later posts fail (the cap limits each entry, not the balance: getting there would take over nine million maximum-size journals, and a post that would overflow fails rather than wrapping around) | [ledger.go](../services/ledger/ledger.go) |
| Posts lock the accounts they touch (`SELECT … FOR UPDATE`, in account ID order) | Two posts racing on one balance, spending the same money twice | [store.go](../services/ledger/store.go) |
| Before a post, the ledger checks every customer account with the account service: it exists, is open, and holds the journal's currency; if the account service can't answer, nothing moves | Money credited to a mistyped or never-opened account, where nobody owns it | [service.go](../services/ledger/service.go) |
| Calls between services use `kit.Get` and `kit.Post`: a 5 second timeout, a 1 MB cap on every response body (errors included), a pool of at most 100 connections to each service, reused rather than re-dialled, a deadline shared down the chain (a caller can ask for at most 30s), and the other service's errors kept from the caller; the ledger checks a journal's accounts at once, so a post waits at most one timeout | A slow or misbehaving service holding up its callers; a burst of calls using up the pod's local ports, so every call fails; another service's internals reaching a caller who didn't call it | [client.go](../pkg/kit/client.go), [service.go](../services/ledger/service.go) |
| Journals and entries are append-only: database triggers refuse `UPDATE`, `DELETE` and `TRUNCATE`, and each journal stores the request ID that posted it | A bug or a stray SQL statement rewriting the history of money; a movement nobody can trace back to its request | [00002_append_only_journals.sql](../services/ledger/migrations/00002_append_only_journals.sql) |
| One final answer per idempotency key: a retry gets the first journal, or the first refusal (kept in `refusals`, append-only), and posts with one key run one at a time under a lock on it; a reused key with a different request is refused | A retried request moving money twice; a late or stalled post moving money after its caller was told it was refused | [store.go](../services/ledger/store.go) |
| Go is given 90% of each container's memory limit (the chart passes the limit in, and `kit.Main` sets Go's limit), and the ledger's limit is sized from a load test | A burst of load pushing memory past the limit, so the pod is killed rather than slowed down | [deployment.yaml](../deploy/charts/service/templates/deployment.yaml), [ledger.yaml](../deploy/values/ledger.yaml) |
| Each transfer is saved, pending, before the ledger is called, and posted with its own ID as the ledger's idempotency key on every attempt, by a request or the background finisher, one attempt at a time (a lease only the claimant holds, and only it may record a refusal); only a refusal fails it, and anything unsure is retried with the same key ([ADR-0010](adr/0010-move-money-as-transfers-saved-before-the-ledger-is-called.md)) | A timeout, a lost reply or a pod dying mid-call moving money twice, or leaving money moved that nobody knows about; two attempts racing, so a transfer is saved as failed although its money moved | [service.go](../services/transaction/service.go), [store.go](../services/transaction/store.go) |
| Completed and failed transfers are final: a trigger refuses any change to them | A bug or stray SQL statement rewriting what happened to a transfer | [00001_create_transfers.sql](../services/transaction/migrations/00001_create_transfers.sql) |
| Panics recovered per request | One bad request crashing the service | [handler.go](../pkg/kit/handler.go) |
| One database per service, with credentials generated by CloudNativePG | One service, or a bug in it, reading another's data | [database.yaml](../deploy/charts/service/templates/database.yaml) |
| Pods run as a non-root UID, with a read-only filesystem, no privilege escalation, all capabilities dropped, the default seccomp profile and no Kubernetes API token | An attacker who gets code running in a pod getting any further | [deployment.yaml](../deploy/charts/service/templates/deployment.yaml) |
| Distroless images with no shell or package manager | Tools an attacker could use inside a container | [Dockerfile](../Dockerfile) |
| Dependencies pinned in `go.mod`; `golang.org/x/net` held at a current version over Typhon's old one | Silently picking up a changed dependency; missing HTTP/2 security fixes | [go.mod](../go.mod), [ADR-0007](adr/0007-build-services-on-typhon-and-terrors.md) |
| Every check runs in CI on every PR, and branch names are checked before push | A change merged without its tests, lint or contract checks | [ci.yml](../.github/workflows/ci.yml) |

## Known gaps

Each gap is known and has a plan. A change must not make one worse in the meantime.

| Gap | Risk today | Closed by |
|---|---|---|
| No sign-in: the account API trusts the customer ID a caller sends, and the transaction API moves money for whoever asks | Any pod in the cluster can open or read accounts, read balances, and move money between any accounts, for anyone | Phase 12: edge gateway, with the customer ID taken from the sign-in token |
| No NetworkPolicies: any pod can call any service | A compromised pod can reach every service | Phase 13: hardening |
| No rate limits | One caller can use up a service's capacity | Phase 13: hardening |
| No load shedding: a service accepts every request, however many are already in flight | Under overload, memory grows until the pod is killed and every caller gets timeouts, instead of the extra requests being turned away quickly (a load test at 400 requests at once killed the ledger at its old 64 MiB limit) | Phase 13: cap requests in flight per service in `kit.Handler`, answering a quick 503 above the cap |
| A caller's `traceparent` is trusted: a service continues whatever trace it's sent | A pod can attach its requests to another request's trace, or make up trace IDs, confusing what Jaeger shows (no effect on money or data) | The edge gateway: it must start a new trace for every request from outside, never continue one a client sent |
| Any pod can post a journal | A compromised or buggy pod can move money between any accounts | Phase 12 (only the transaction service may post) and Phase 13 (NetworkPolicies) |
| The ledger's database user owns its tables, so it could drop the append-only triggers | Someone with the service's database password could still rewrite history | Phase 13: a separate owner role for migrations, and a runtime role with only `INSERT` and `SELECT` on journals and entries |
| A revealed secret is an ordinary string again | Code that puts the value from `Reveal()`, or `string(s)`, into an error or log leaks it | Review: each `Reveal()` call is checked ([ADR-0008](adr/0008-stop-secrets-where-they-enter.md)) |
| No TLS between services, or to Postgres or Jaeger | Traffic inside the local cluster, including spans sent to Jaeger, is readable by anything on it | Not planned for the local cluster |
| Kafka has no auth, TLS or topic ACLs: one plaintext listener, inside the cluster only | Any pod can read or write any topic, and read messages in transit (no service writes to Kafka yet) | Hardening: TLS, a user per service (SCRAM or mTLS) and topic ACLs, with NetworkPolicies |
| No automated check for known-vulnerable dependencies (such as `govulncheck`) | A vulnerable dependency could go unnoticed | Not planned yet |
| Local Jaeger UI has no login | Anyone with access to the developer's machine can read traces (once services send them: route names, timings, errors; never personal data) | Local only, by design: a `ClusterIP` Service, reachable only through `kubectl port-forward` on localhost |
| Local Grafana lets anyone in as admin | Anyone with access to the developer's machine can change dashboards | Local only, by design: reachable only through `kubectl port-forward` |
