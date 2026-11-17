# Phase 7 — Ledger (double-entry)

## What we built

- **The ledger service** ([services/ledger](../../services/ledger/), [cmd/ledger](../../cmd/ledger/main.go)): the one record of every movement of money, in its own Postgres database (`ledger-db`). The design and the alternatives are in [ADR-0009](../adr/0009-record-money-as-double-entry-journals-with-locked-balances.md).
  - **Journals** of 2 to 10 entries in one currency, adding up to zero ([ledger.go](../../services/ledger/ledger.go)). Each entry is capped at 10¹² minor units.
  - **Funding accounts**, one per currency with a fixed ID, where money from outside the bank comes from. They may go negative; customer accounts can't.
  - **Row locks** (`SELECT … FOR UPDATE`, in account ID order) around every post, with all the locks taken in one query and all the writes sent in one batch ([store.go](../../services/ledger/store.go)).
  - **Idempotency keys**: a retry gets the first journal back, and a reused key for a different request is a 412.
  - **Account checks** with the account service before money moves: each account must exist, be open and hold the journal's currency ([service.go](../../services/ledger/service.go)).
  - **An append-only audit trail**: triggers refuse `UPDATE`, `DELETE` and `TRUNCATE` on journals and entries, and each journal records the request ID that posted it ([00002_append_only_journals.sql](../../services/ledger/migrations/00002_append_only_journals.sql)).
- **The ledger's API** ([ledger.proto](../../proto/ledger/v1/ledger.proto)): `POST /journals` (201, or 200 for a retry), `GET /journals/:id` and `GET /accounts/:id/balance`, on `localhost:8082` under `make dev`. Amounts are minor units, and come back as strings, as proto3 JSON writes every 64-bit integer.
- **The first call between services** ([client.go](../../pkg/kit/client.go)): `kit.Get` asks another service for binary protobuf, passes the request ID on, gives each call 5 seconds, caps every response body at 1 MB, and reads leniently. `kit.Respond` answers in binary when asked. The ledger turns the account service's errors into its own, so they never reach its callers.
- **Business metrics and a dashboard** ([metrics.go](../../services/ledger/metrics.go), [tiny-bank / Ledger](../../deploy/monitoring/dashboards/tiny-bank-ledger.json)): `ledger_journals_total{currency, outcome, reason}` and `ledger_moved_minor_units_total{currency}`. The posted, retried and failed series, and money moved, start at 0 for every currency. Refusal series start on first use.
- **Shared startup for database services** ([pkg/db/run.go](../../pkg/db/run.go)): the second database service showed the wiring was the same every time, so it moved into shared code.
  - `db.Run` is a whole service: config, logs, routes, then waiting for the database, migrating, and serving.
  - `kit.Handler` adds the probes, and `kit.ParseUUID` accepts only canonical IDs.
  - A service's `cmd/` is now its migrations and a `routes(router, db.Deps)` function.

**What waits for later.** Binary *request* bodies arrive with the first service-to-service POST, the transaction service calling the ledger in Phase 8. Retries with backoff are the caller's job, also Phase 8. Who may post (only the transaction service) waits for sign-in in Phase 12. Separate database roles, so the ledger's own user can't drop the append-only triggers, are Phase 13.

## Concepts

- **Double-entry bookkeeping.** Every movement of money takes from one account and adds to another, so a journal's entries always sum to zero. Money is never created or lost by accident, because the books can't balance if it is. Two checks follow: every currency's balances sum to zero, and every balance equals the sum of its entries. The tests run both after every store test.
- **Where outside money comes from.** A deposit can't add money from nowhere, so it comes from a funding account: −£10 from funding, +£10 to Alice. The funding account's negative balance is how much money has come into the bank.
- **Locks stop a race; their order stops a deadlock.** Without `FOR UPDATE`, two posts can both read a balance of £30, both decide there's enough, and both take £30. With it, the second waits for the first to commit. If one post locked Alice then Bob while another locked Bob then Alice, each would wait for the other for ever. Locking in account ID order makes every post queue in the same order. `TestConcurrentPostsNeverOverdraw` and `TestOppositeTransfersDontDeadlock` fail without each.
- **Idempotency.** A caller whose request timed out can't tell whether the money moved. If it retries with the same key, the ledger returns the first journal instead of moving the money again, so retrying is always safe. A retry that overlaps the first request, and whose account check fails, still gets the first journal: once money has moved, no answer may say it didn't.
- **Check outside the lock.** Calling another service inside a transaction would hold the account locks and a database connection for as long as that service takes. So the account checks run first, all at once, and the transaction only does database work.
- **Don't pass another service's errors on.** A caller of the ledger didn't call the account service, so it gets the ledger's own `not_found.account` or `internal_service.account_service`, never the account service's codes or messages. The full error is still logged.
- **Append-only history.** Like a paper ledger written in ink, entries are never rubbed out. A mistake is fixed with a new journal that reverses it, so the history shows both.
- **Business metrics need care with counters.** A counter that first appears at 300 hides that increase from Prometheus, which has no earlier sample to compare. So every series the ledger can name in advance starts at 0: money moved, and posts posted, retried or failed, for every currency. A refusal's reason can't all be listed in advance, so each reason's series appears on its first refusal, and that first one doesn't show in `rate()` or `increase()`. Labels stay a small, fixed set: currencies, outcomes and terror codes, never account IDs or amounts.

## How to verify

```bash
make check
```

```bash
make test-integration
```

This needs Docker. The ledger's tests run against Postgres, including the concurrency tests, and the end-to-end test runs the ledger against a real account service, so it checks every account and passes the request ID on.

With `make dev` running, open two accounts with the account service:

```bash
ALICE=$(curl -s -X POST localhost:8081/accounts -H 'Content-Type: application/json' -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Alice","currency":"GBP"}' | jq -r .id); echo $ALICE
```

```bash
BOB=$(curl -s -X POST localhost:8081/accounts -H 'Content-Type: application/json' -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000002","holder_name":"Bob","currency":"GBP"}' | jq -r .id); echo $BOB
```

Deposit £10 into Alice's account from the GBP funding account:

```bash
curl -s -X POST localhost:8082/journals -H 'Content-Type: application/json' -d "{\"idempotency_key\":\"deposit-$ALICE\",\"currency\":\"GBP\",\"entries\":[{\"account_id\":\"00000000-0000-4000-8000-000000000826\",\"amount\":-1000},{\"account_id\":\"$ALICE\",\"amount\":1000}]}" | jq
```

Move £3 to Bob. Send it twice: the second answer is a 200 with the same journal, and nothing more moves.

```bash
curl -si -X POST localhost:8082/journals -H 'Content-Type: application/json' -d "{\"idempotency_key\":\"t1-$ALICE\",\"currency\":\"GBP\",\"entries\":[{\"account_id\":\"$ALICE\",\"amount\":-300},{\"account_id\":\"$BOB\",\"amount\":300}]}"
```

Alice now has 700 and Bob 300:

```bash
curl -s localhost:8082/accounts/$ALICE/balance | jq; curl -s localhost:8082/accounts/$BOB/balance | jq
```

The [tiny-bank / Ledger](http://localhost:3000/d/tiny-bank-ledger) dashboard shows the posts within a minute. To give it steady traffic, run the load generator ([tools/journalload](../../tools/journalload/main.go)). It opens its own accounts, funds them, then posts random transfers, some of them retries and some refused for insufficient funds, and prints a summary of results and latency:

```bash
make load-journals RATE=20 DURATION=2m
```

Other settings, such as how much each account starts with or how many requests are retries, go in `ARGS`; `make load-journals ARGS=-h` lists them.
 To see the audit trail refuse a change:

```bash
kubectl --context kind-tiny-bank exec -it ledger-db-1 -c postgres -- psql app -c "DELETE FROM entries"
```

It answers `entries is append-only: DELETE is not allowed`.
