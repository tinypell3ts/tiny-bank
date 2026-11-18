# Phase 8 — Transaction service and tracing

Phase 8 comes in two parts. **8a** is the transaction service. **8b** adds tracing with OpenTelemetry and Jaeger, so one request can be followed across every service, query and retry it touches.

## What we built (8a)

- **The transaction service** ([services/transaction](../../services/transaction/), [cmd/transaction](../../cmd/transaction/main.go)): it takes requests to move money and sees each one through, in its own Postgres database (`transaction-db`). Why it works this way: [ADR-0010](../adr/0010-move-money-as-transfers-saved-before-the-ledger-is-called.md).
  - **Transfers as a state machine** ([transfer.go](../../services/transaction/transfer.go), [store.go](../../services/transaction/store.go)): `pending`, then `completed` with the ledger's journal or `failed` with its reason. A trigger refuses any change to a finished transfer ([00001_create_transfers.sql](../../services/transaction/migrations/00001_create_transfers.sql)).
  - **Posting to the ledger** ([ledger.go](../../services/transaction/ledger.go), [service.go](../../services/transaction/service.go)): in binary protobuf with `kit.Post`, keyed by the transfer's own ID. A request tries up to 3 times with jittered backoff, inside its caller's deadline.
  - **One attempt at a time**: an attempt claims the transfer first and gets a lease ([00003_lease_transfers.sql](../../services/transaction/migrations/00003_lease_transfers.sql)), so a retry and the finisher never post the same transfer at once.
  - **The finisher**: every pod claims due pending transfers each second (`FOR UPDATE SKIP LOCKED`) and tries each again, backing off from 10 seconds to 5 minutes. It runs through a new `db.Deps.Background` hook, which starts work after the database is migrated and waits for it at shutdown ([pkg/db/run.go](../../pkg/db/run.go)).
  - **Deposits** come from the currency's funding account (now shared, [pkg/funding](../../pkg/funding/funding.go)).
- **The transaction API** ([transaction.proto](../../proto/transaction/v1/transaction.proto)): `POST /transfers`, `POST /deposits` and `GET /transfers/:id`, on `localhost:8083` under `make dev`. A new transfer gets 201 if it finished in the request and 202 while pending; a retry with the same idempotency key gets 200 and the same transfer. A refusal is a transfer with `state: TRANSFER_STATE_FAILED` and a `failure_reason`, not an error.
- **Shared code in `pkg/kit`** for the second service that needed it: strict binary request decoding in `kit.Decode`, `kit.Post`, a deadline shared down the chain (`X-Request-Timeout`, with 100 ms kept back at each hop), `kit.CheckIdempotencyKey`, `kit.Refusal`, `kit.ServiceURL`, and `kittest.Serve` for tests that run several services.
- **Business metrics and a dashboard** ([metrics.go](../../services/transaction/metrics.go), [tiny-bank / Transactions](../../deploy/monitoring/dashboards/tiny-bank-transaction.json)): ledger posts by who tried and by outcome, and how many transfers are pending and for how long.

**What waits for later.** Tracing came next, in 8b (below). Alerts on stuck transfers are Phase 13. Only the transaction service being allowed to post to the ledger waits for sign-in (Phase 12).

## Concepts (8a)

- **Save before you call.** If the service called the ledger first and then saved, a crash in between would leave money moved that nobody here knows about. Saved first, the worst case is a pending transfer, which the finisher completes.
- **"Unsure" is its own answer.** A timeout doesn't mean "no". The ledger may have posted the journal and lost the reply. So only an answer that means no (a 4xx refusal) fails a transfer. Anything else is retried with the same key, and the ledger answers a repeat with the journal it already posted. `TestALostReplyIsReplayedWithTheSameKey` plays this out.
- **One answer per key, kept where the money is.** If two attempts ran at once, one could be refused, a deposit land, and the other post the money, and "failed" might be saved last. A lease here can't fully stop that: a worker can pause after checking its lease and post much later. So the ledger keeps a refusal against its key, as it keeps a journal, and the first answer for a key is the only one (`TestAKeyGetsOneAnswerUnderARace`, `TestARefusedKeyStaysRefused`). The check has to live with the money, like a fencing token in a distributed lock.
- **One attempt at a time.** On top of that, an attempt claims the transfer (only when it's due) and gets a random lease, like a version number in optimistic locking, so a retry and the finisher don't post the same transfer twice for nothing. `TestTwoAttemptsNeverPostAtOnce` plays it out.
- **Jitter.** If the ledger blips and a thousand transfers all retry exactly 50 ms later, they hit it together again. Waiting a random time up to the backoff ("full jitter") spreads them out.
- **A deadline shared down the chain.** The caller's remaining time travels in `X-Request-Timeout`. Each hop keeps 100 ms back, so it gives up, and answers, just before its caller does, instead of everyone timing out at once with nobody answering.
- **A job queue in a table.** `SELECT … FOR UPDATE SKIP LOCKED` lets several finishers take rows from the same table without waiting for each other or taking the same row twice. Claiming a row pushes its `next_attempt_at` 30 seconds ahead, a lease: if the pod dies holding it, the transfer is due again when the lease runs out. `TestFinishersNeverClaimTheSameTransfer` runs four at once.
- **Stopping cleanly.** At shutdown the finisher's context ends, it finishes the transfer it's on, and `db.Run` waits for it before closing the database, so it never loses its connection mid-write.
- **Watch what's stuck, not just what failed.** A transfer pending for 10 minutes is a bigger problem than a refused one, so the dashboard leads with the pending count and the oldest one's age.

## How to verify (8a)

```bash
make check
```

```bash
make test-integration
```

This needs Docker. The end-to-end test runs the transaction service against a real ledger and account service.

With `make dev` running, open two accounts as in [Phase 7](07-ledger.md#how-to-verify) (`$ALICE` and `$BOB`), then deposit £10 into Alice's account:

```bash
curl -si -X POST localhost:8083/deposits -H 'Content-Type: application/json' -d "{\"idempotency_key\":\"d-$ALICE\",\"account_id\":\"$ALICE\",\"currency\":\"GBP\",\"amount\":1000}"
```

Move £3 to Bob. Send it twice: the first answer is a 201 with a completed transfer, the second a 200 with the same one.

```bash
curl -si -X POST localhost:8083/transfers -H 'Content-Type: application/json' -d "{\"idempotency_key\":\"t1-$ALICE\",\"from_account_id\":\"$ALICE\",\"to_account_id\":\"$BOB\",\"currency\":\"GBP\",\"amount\":300}"
```

Ask for more than Alice has: it's a 201 with a failed transfer, `failure_reason` `precondition_failed.insufficient_funds`.

```bash
curl -s -X POST localhost:8083/transfers -H 'Content-Type: application/json' -d "{\"idempotency_key\":\"t2-$ALICE\",\"from_account_id\":\"$ALICE\",\"to_account_id\":\"$BOB\",\"currency\":\"GBP\",\"amount\":5000}" | jq
```

To watch the finisher, stop the ledger, send a transfer (it answers 202, pending), then start the ledger again. Within about a minute (the finisher's next attempt) `GET /transfers/<id>` shows it completed, and the [tiny-bank / Transactions](http://localhost:3000/d/tiny-bank-transaction) dashboard shows the pending count rise and fall.

```bash
kubectl --context kind-tiny-bank scale deploy/ledger --replicas=0
```

```bash
kubectl --context kind-tiny-bank scale deploy/ledger --replicas=1
```

## What we built (8b)

8b came as a stack of small PRs (#16 to #25), each one concern. The decision and its costs are in [ADR-0011](../adr/0011-trace-requests-with-opentelemetry-and-jaeger.md).

- **Jaeger on the cluster** ([deploy/jaeger/values.yaml](../../deploy/jaeger/values.yaml), `make jaeger`): Jaeger v2, all-in-one, built on the OpenTelemetry Collector. It receives traces over OTLP, keeps them in memory, and shows them at [localhost:16686](http://localhost:16686). The pod has the same hardening as ours, and the UI is port-forwarded to localhost only.
- **Tracing set up once, for every service** ([pkg/obs/tracing.go](../../pkg/obs/tracing.go)): `kit.Main` calls `obs.StartTracing`, which sends spans in the background over OTLP/HTTP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (the chart sets it for every service) and does nothing when it isn't. At shutdown it sends what's buffered, waiting at most 3 seconds. `obs.NewTracer` gives each package a tracer that follows the global provider, and `obs.FailSpan` is the one way to mark a span failed.
- **A server span per request** ([pkg/kit/tracing.go](../../pkg/kit/tracing.go)): a filter in `kit.Handler`, second in the chain, continues the caller's `traceparent` or starts a trace. Spans are named and labelled like the metrics (`POST /transfers`, never a raw path), only a 5xx marks one failed, and probes get none. The access log carries `trace_id` next to `request_id`. Every service's `TestConformance` checks it.
- **A client span per call, with the trace passed on**: `kit.Get` and `kit.Post` give each call a span named by the other service (`POST ledger`) and send `traceparent`, so one request is one trace across services. A call that got no answer is marked `_OTHER`, not the 500 Typhon makes up for it.
- **A span per query** ([pkg/db/tracing.go](../../pkg/db/tracing.go)): every pool `db.Open` makes traces each query inside a request, named by operation and table (`UPDATE transfers`, `BATCH INSERT accounts, SELECT accounts FOR UPDATE`), with its SQL text but never its values.
- **The finisher's attempts**: each is a trace of its own, `finish transfer`, holding `transfer.id`, its state after the attempt, and the trace that created it.
- **Transfers keep their trace**: each transfer saves the trace ID and request ID of the request that created it, so support can go from a customer's payment to its trace and logs in one query ([docs/observability.md](../observability.md#finding-a-customers-transfer)).

**What waits for later.** Sampling, once traffic is too high to keep every span. Trace context through Kafka with the outbox (Phase 10). Alerts on the numbers traces reveal (Phase 13). The edge gateway starting a fresh trace for every request from outside, rather than trusting a client's `traceparent` (Phase 12).

## Concepts (8b)

- **Metrics say that, traces say where, logs say why.** The error rate rose at 14:02 (Grafana). A failing trace from 14:02 shows which hop and which query (Jaeger). That request's logs have the full error: every line logged during the request carries its trace ID, so one `kubectl logs | grep <trace_id>` finds them all, the error included. The `trace_id` on each line is what joins traces to logs.
- **A trace is a tree of spans.** Each span is one piece of work, with a start, an end, a name and a parent. The trace ID travels between services in one header, `traceparent: 00-<trace id>-<parent span>-01`, and inside a service in Go's `context.Context`, which is why every call passes `req.Context`.
- **Name spans like metric labels.** A span name or attribute from a raw path or a value would make unlimited names and could carry personal data. Route patterns, service names, table names and fixed codes are a small set, and never hold what a caller typed. A query's span holds its SQL, which is always fixed text with `$1`, never its values.
- **Trace work, not polling.** Probes, the finisher's once-a-second check for due transfers, and migrations would bury real traffic, so only requests, and the finisher's actual attempts, make spans.
- **A refusal isn't a server error.** A 4xx is the caller's mistake, so a server span isn't marked failed for it. A client span is, because from the caller's side the call didn't get what it asked for.
- **Record only what really happened.** When a connection fails, Typhon invents a 500 response. The client span records a status only for a response that came back, so an unreachable service shows as `_OTHER`, not as a service that answered 500.
- **A trace is one request; background work links back to it.** The finisher retries minutes later, outside any request, so each attempt is a trace of its own that points back to the creating trace by ID, like a redelivery card quoting the original parcel number.
- **Start from the record, not the trace.** A customer doesn't know a trace ID. Saving it on the transfer means support starts from their payment and follows it.

## How to verify (8b)

```bash
make check
```

```bash
make test-integration
```

`TestADepositIsOneTrace` runs all three services and checks one deposit is one trace, each span under the one that called it. Each service's `TestConformance` checks its server spans.

With `make dev` running, Jaeger's UI is at [localhost:16686](http://localhost:16686). Send a deposit with a known trace ID and read its trace:

```bash
A=$(curl -s -X POST localhost:8081/accounts -H 'Content-Type: application/json' -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}' | jq -r .id)
T=$(openssl rand -hex 16)
curl -s -o /dev/null -w "%{http_code}\n" -X POST localhost:8083/deposits -H 'Content-Type: application/json' -H "traceparent: 00-$T-$(openssl rand -hex 8)-01" \
  -d "{\"idempotency_key\":\"trace-$A\",\"account_id\":\"$A\",\"currency\":\"GBP\",\"amount\":1000}"
sleep 6   # spans are sent in batches, about every 5 seconds
curl -s localhost:16686/api/traces/$T | jq -r '.data[0] as $t | $t.spans | sort_by(.startTime) | .[] | "\($t.processes[.processID].serviceName)\t\(.operationName)"'
```

It lists the deposit's spans across all three services: `POST /deposits`, the transaction service's `INSERT transfers` and `UPDATE transfers`, `POST ledger`, the ledger's `POST /journals` with its `BEGIN`, account locks, writes and `COMMIT`, and the account service's `GET /accounts/:id`. Paste `$T` into *Lookup by Trace ID* to see it as a timeline.

To find a customer's transfer and its trace the way support would, see [Finding a customer's transfer](../observability.md#finding-a-customers-transfer). To see the finisher, stop the ledger as in 8a above, send a transfer, start the ledger again, then search Jaeger for service `transaction`, operation `finish transfer`.

