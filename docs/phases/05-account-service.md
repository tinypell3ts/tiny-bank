# Phase 5 — The account service

> Phase 5.5 moved this service onto Typhon and terrors: errors are now terrors (`bad_request.holder_name`, `not_found.account`), the strict-input checks live in `kit.DecodeJSON`, and a wrong content type or a too-large body is a 400 with its own code instead of 415 or 413. This page records Phase 5 as it was built.

## What we built

- **The account service** (`cmd/account` for wiring, `services/account` for the code). It opens accounts and looks them up, and stores them in its own Postgres database, `account-db`.
- **The `accounts` table** (`services/account/migrations/00001_create_accounts.sql`):
  - an `id` and a `customer_id`, both UUIDs;
  - a `holder_name` of 1–200 characters;
  - a `currency`, limited to the four `pkg/money` supports;
  - a `status` (open);
  - `created_at`.

  There's no balance column: balances will come from the ledger (Phase 7).
- **The domain and the store** (`account.go`, `store.go`):
  - **`NewRequest.Validate`** is the one place a name's rules live, and returns a `ValidationError` whose `Reason` is safe to show callers.
  - **`Store.Create` and `Store.Get`** use only query parameters.
  - **IDs** are Go's standard-library `uuid.UUID`, so the compiler stops text being passed where an ID belongs.
- **The HTTP API** (`http.go`):
  - **`POST /accounts`** answers 201 with a `Location` header.
  - **`GET /accounts/{id}`** answers 200, or 404 for a missing account and for anything that isn't a canonical UUID.
  - **Strict input:** JSON only (else 415), one object, no unknown fields, at most 16 KB (else 413).
  - **Errors:** a short, safe message. Unexpected errors are logged with the request ID and answered with a generic 500. A caller who hangs up gets a 499, which isn't counted as a server error.
- **Startup in the right order.** The service connects to its database, retrying for up to 2 minutes while a brand-new one comes up, then applies its migrations, and only then serves. `/readyz` checks the database on every probe, while `/healthz` never does. The shared chart gained a **startup probe**, so a slow start isn't mistaken for a hung process.
- **`pkg/kit`**, the shared foundation, used by `hello` and `account`:
  - `Main`, `Config`/`LoadConfig` and `NewLogger`;
  - `Serve`, with graceful shutdown that cancels requests still stuck at the deadline;
  - `RequestID`, `AccessLog`, `WriteJSON` and `Healthz`.

  It was extracted as soon as a second service needed the same code, and `hello`'s tests moved with it.
- **`pkg/db/dbtest`**: `StartPostgres` for integration tests, shared by `pkg/db` and the account service.
- **On the cluster:** `deploy/values/account.yaml` turns on `database.enabled`, and the Tiltfile has one `go_service('account', port=8081)` line. The Tilt dashboard groups each service with its database.
- **The plan:** onboarding comes before accounts, as **Phase 11.5**, a customer service. In Phase 12 the customer ID will come from the sign-in token, not from what the caller sends. Recorded in [ADR-0006](../adr/0006-onboard-customers-before-accounts.md).

## Concepts

- **Wiring versus code.** `cmd/<name>` only puts things together: config, startup order and routes. The service's rules and storage live in `services/<name>`, where they can be tested without a running server.
- **The rule of two.** Copying `hello`'s server once to start `account` was fine. Keeping two copies would mean a fix reaches only one of them, so the shared parts moved into `pkg/kit` straight away.
- **Validate once, at the edge.** Untrusted text becomes typed values as soon as it arrives: a `uuid.UUID`, a `money.Currency`, a checked `NewRequest`. Everything after that can rely on them. The database keeps a simple length check as a safety net, and doesn't copy every rule.
- **Typed IDs.** `uuid.UUID` is 16 bytes, not text. Passing a name where an ID is expected is a compile error, and two spellings of the same ID compare equal. The API accepts only the canonical form, so each account has exactly one URL.
- **Errors that are safe to show.** `ValidationError.Reason` is written for the caller. `err.Error()` carries internal context such as "create account:", and only goes in logs. That's AGENTS.md's "never leak internal errors to API clients".
- **The repository pattern.** The HTTP handler depends on the `Accounts` interface, not on Postgres. Tests pass an in-memory fake, and production passes `*Store`. This interface exists because a test needs it.
- **Startup, readiness and liveness probes.**
  - **Startup:** "give me time to start".
  - **Readiness:** "send me traffic", which fails if the database doesn't answer, so traffic stops without a restart.
  - **Liveness:** "am I alive", which never checks dependencies, or a database blip would restart every pod.
- **Graceful shutdown with a deadline.** In-flight requests may finish. Any still running when the time is up are cancelled, through `http.Server.BaseContext`, so their database connections are released and the pool can close.
- **One database per service, owned by its release.** `account-db` is created, named and deleted with the account service, and only it has the credentials. Tearing the service down (`make dev-down`, `make undeploy`) deletes the database's data, because its storage is owned by the `Cluster`.
- **Customers come before accounts.** In a bank, onboarding (identity checks, terms) creates the customer, and accounts are opened for them afterwards. The account API is internal and names the customer explicitly. Customers will never send their own ID: the edge gateway will take it from who's signed in ([ADR-0006](../adr/0006-onboard-customers-before-accounts.md)).
- **Testing that the tests test something.** Each guard was checked by putting the bug back and watching a test fail. Twice that showed a test proving nothing: once because the failure was hidden by timing, and once because a deferred call did the work.

## How to verify

```bash
make check
```

```bash
make test-integration
```

This needs Docker. Look for `cmd/account ... 92.6%` and `services/account ... 96.2%`. The run includes the end-to-end API test against real Postgres.

```bash
make dev
```

The Tilt dashboard (localhost:10350) shows an **account** group with `account` and `account-db`. The account pod may log "database not ready, retrying" for a few seconds on the first run, then "database migrated", with no restarts. Then:

```bash
curl -i -X POST localhost:8081/accounts -H 'Content-Type: application/json' -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada Lovelace","currency":"GBP"}'
```

You should get `201 Created` with a `Location` header. `curl localhost:8081<Location>` returns the same account, and `curl localhost:8081/accounts/not-a-uuid` returns 404. In Grafana's *tiny-bank / Services (RED)* dashboard, pick `account` in the service picker to see its traffic.

Stopping Tilt with Ctrl-C keeps the database. `make dev-down` deletes it, with every account in it.
