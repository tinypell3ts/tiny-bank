# Phase 8 — Transaction service and tracing

Phase 8 comes in two parts. **8a**, below, is the transaction service. **8b** adds tracing with OpenTelemetry and Jaeger, so one request can be followed across all three services.

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

**What waits for later.** Tracing is 8b. Alerts on stuck transfers are Phase 13. Only the transaction service being allowed to post to the ledger waits for sign-in (Phase 12).

## Concepts

- **Save before you call.** If the service called the ledger first and then saved, a crash in between would leave money moved that nobody here knows about. Saved first, the worst case is a pending transfer, which the finisher completes.
- **"Unsure" is its own answer.** A timeout doesn't mean "no". The ledger may have posted the journal and lost the reply. So only an answer that means no (a 4xx refusal) fails a transfer. Anything else is retried with the same key, and the ledger answers a repeat with the journal it already posted. `TestALostReplyIsReplayedWithTheSameKey` plays this out.
- **One answer per key, kept where the money is.** If two attempts ran at once, one could be refused, a deposit land, and the other post the money, and "failed" might be saved last. A lease here can't fully stop that: a worker can pause after checking its lease and post much later. So the ledger keeps a refusal against its key, as it keeps a journal, and the first answer for a key is the only one (`TestAKeyGetsOneAnswerUnderARace`, `TestARefusedKeyStaysRefused`). The check has to live with the money, like a fencing token in a distributed lock.
- **One attempt at a time.** On top of that, an attempt claims the transfer (only when it's due) and gets a random lease, like a version number in optimistic locking, so a retry and the finisher don't post the same transfer twice for nothing. `TestTwoAttemptsNeverPostAtOnce` plays it out.
- **Jitter.** If the ledger blips and a thousand transfers all retry exactly 50 ms later, they hit it together again. Waiting a random time up to the backoff ("full jitter") spreads them out.
- **A deadline shared down the chain.** The caller's remaining time travels in `X-Request-Timeout`. Each hop keeps 100 ms back, so it gives up, and answers, just before its caller does, instead of everyone timing out at once with nobody answering.
- **A job queue in a table.** `SELECT … FOR UPDATE SKIP LOCKED` lets several finishers take rows from the same table without waiting for each other or taking the same row twice. Claiming a row pushes its `next_attempt_at` 30 seconds ahead, a lease: if the pod dies holding it, the transfer is due again when the lease runs out. `TestFinishersNeverClaimTheSameTransfer` runs four at once.
- **Stopping cleanly.** At shutdown the finisher's context ends, it finishes the transfer it's on, and `db.Run` waits for it before closing the database, so it never loses its connection mid-write.
- **Watch what's stuck, not just what failed.** A transfer pending for 10 minutes is a bigger problem than a refused one, so the dashboard leads with the pending count and the oldest one's age.

## How to verify

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
