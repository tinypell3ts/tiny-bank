# Phase 6 — Protobuf contracts

## What we built

- **The account contract** ([account.proto](../../proto/account/v1/account.proto)): `Account`, `CreateAccountRequest` and an `AccountStatus` enum, in the versioned package `account.v1`. Every message and field has a comment saying what it means and what values are allowed, and the file lists the routes and error codes.
- **buf** checks every contract ([buf.yaml](../../proto/buf.yaml)): the STANDARD lint rules for naming, and the strict FILE rules for breaking changes.
- **Generated Go, committed** ([account.pb.go](../../proto/account/v1/account.pb.go)), so building a service or its image doesn't need buf. `protoc-gen-go` is pinned as a Go tool in `go.mod`, so everyone generates exactly the same code.
- **Two make targets:**
  - `make proto` regenerates the Go after a `.proto` changes.
  - `make proto-check` fails if a contract is badly formatted, breaks the naming rules, would break callers compared with `main`, or doesn't match its committed Go code. It's part of `make check`, and a new CI job runs it against `origin/main`.
- **`kit.Decode` and `kit.Respond`** ([decode.go](../../pkg/kit/decode.go), [respond.go](../../pkg/kit/respond.go)) read a request into a proto message and write one back, as JSON with the proto field names (`customer_id`).
  - `Decode` replaced `DecodeJSON`, with the same strict checks.
  - `Respond` never sends an empty 201 when encoding fails.
- **The account service speaks the contract.** Its handlers read `CreateAccountRequest` and answer with `Account`, converting to and from the service's own types at the edge.
- **Rules for changing contracts:** [proto/AGENTS.md](../../proto/AGENTS.md).

**What callers see differently.** An account's status is `"ACCOUNT_STATUS_OPEN"` instead of `"open"`. Field names, routes, status codes and errors are unchanged.

**What waits for Phase 7.** Binary protobuf, the Typhon client for the account service, and passing the request ID on to the service being called all arrive with the first service-to-service call, when the ledger calls the account service. They were cut from this phase because nothing would use them yet.

## Concepts

- **A contract is a promise to every caller**, including ones deployed before your change. Services are updated and rolled back in any order, so a contract must keep working both ways. Writing it in one `.proto` file, rather than in Go structs and docs, gives buf something to check.
- **Field numbers are what go over the wire; names are for people and JSON.** Binary protobuf sends `2: "Ada"`, never `holder_name`. So changing a field's number corrupts data between old and new services, and is never allowed. Our JSON uses the names, so renaming a field breaks JSON callers too. buf's FILE rules catch both.
- **Generated code is committed, and checked for drift.** Committing it keeps `go build` and the Docker image free of buf. The cost is that someone could change a `.proto` and forget `make proto`. So `proto-check` generates into `build/_proto-check` and compares. Go ignores directories starting with `_`, so the copy isn't mistaken for a second package.
- **Strict requests set the order of a change.** `kit.Decode` refuses a field it doesn't know, so a typo like `currancy` is a 400 instead of being silently ignored. The price is that "just add a field" has an order:
  - **A new request field:** deploy the service that reads it before any caller sends it.
  - **Retiring a field:** stop every caller sending it before deleting it.
  - **A new response field or enum value:** safe, because callers read responses leniently.

  buf compares files, not deployments, so a PR says which service ships first.
- **Strict in, lenient out.** A service is strict about what it accepts and generous about what it reads back from others. When the account client arrives in Phase 7, it will ignore fields and enum values it doesn't know, so the account service can add them without breaking the ledger.
- **Enums start with `UNSPECIFIED`.** proto3 doesn't send a field left at its zero value, so "not set" and "the first value" look the same. A zero value that means "not set" keeps a forgotten status from looking like a real one. Callers treat a value they don't recognise as the safe choice, which for an account is "not open".
- **Proto types stay at the edge.** Handlers convert between proto messages and the service's own types (`uuid.UUID`, `money.Currency`, `Status`). The domain and the store never import generated code, so the wire format can change without touching business rules.
- **JSON naming is a decision, not a default.** Typhon writes proto JSON in lowerCamelCase (`customerId`), protobuf's standard JSON style. `kit.Respond` uses the proto names instead, so the API kept the names it already had and matches its errors and database columns. One helper in the kit makes every service do the same.
- **Build for a caller, not for a maybe.** The first version of this phase also decoded and answered in binary protobuf, picking between the two from the `Accept` header. A review found none of it had a caller. It would have been maintained, reviewed and tested for traffic that doesn't exist, and its design would have been a guess about the Phase 7 client. Cutting it roughly halved the kit's new code.

## How to verify

```bash
make proto-check
```

This formats and lints the contract, compares it with `main` for breaking changes (skipped while `main` has no contracts), and checks the committed Go code matches it.

```bash
make check
```

```bash
make test-integration
```

This needs Docker. It runs the real account service against Postgres, opening an account and reading it back.

To see the breaking-change check work, renumber a field in [account.proto](../../proto/account/v1/account.proto), say `currency = 4` to `currency = 9`, and compare against the file before your change:

```bash
buf breaking proto --against '.git#ref=HEAD,subdir=proto'
```

It reports that field 4 was deleted. Undo the change afterwards.

With `make dev` running:

```bash
curl -s -X POST localhost:8081/accounts -H 'Content-Type: application/json' -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}'
```

The answer has `customer_id`, `holder_name` and `"status": "ACCOUNT_STATUS_OPEN"`. Change `currency` to `currancy` to get a 400 with `request body has an unknown field "currancy"`.
