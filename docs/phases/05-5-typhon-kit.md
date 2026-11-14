# Phase 5.5 — The kit on Typhon and terrors

## What we built

- **The decision** ([ADR-0007](../adr/0007-build-services-on-typhon-and-terrors.md)): services are built on [Typhon](https://github.com/monzo/typhon) and return [terrors](https://github.com/monzo/terrors) errors, instead of our own framework now and gRPC later. Services will call each other over HTTP with Typhon clients, with protobuf contracts in Phase 6.
- **`kit.Handler`** ([handler.go](../../pkg/kit/handler.go)): a service gives it a `typhon.Router`, and gets back the `http.Handler` that `kit.Serve` runs. Every request goes through the same filters:
  - request IDs;
  - an access log line;
  - RED metrics by route pattern;
  - Typhon's `ErrorFilter`;
  - error cleaning;
  - panic recovery.

  `/metrics` is served alongside, and every request body is capped at 16 KB.
- **Safe errors.** Typhon on its own sends a terror's stack trace, with file paths from the server, back to the caller. It also turns a raw error's text (a database error, say) into the message. The kit's error-cleaning filter sends only the code, message and params, logs anything internal with the request ID, and replaces it with a generic 500.
- **`kit.Secret`** ([secret.go](../../pkg/kit/secret.go)): every secret from the environment, such as `DATABASE_URL`, prints as `xxxxx` through `fmt`, `log/slog`, errors and JSON. `Reveal()` is the one way to get the value, used once, for `db.Open`.
- **`kit.DecodeJSON`** ([decode.go](../../pkg/kit/decode.go)): request bodies must be one JSON object, with no unknown fields. Every failure is a 400 with its own code and a message that says what's wrong:
  - `bad_request.content_type`
  - `bad_request.too_large`
  - `bad_request.invalid_json`, such as `request body has an unknown field "currancy"`
  - `bad_request.unreadable_body`
- **Both services moved over.**
  - **`hello`**'s routes are Typhon handlers.
  - **The account service**'s handlers now contain only account logic. Its domain returns terrors: `bad_request.holder_name`, `bad_request.customer_id`, `bad_request.currency` and `not_found.account`.
- **`kittest.Conformance`** ([kittest.go](../../pkg/kit/kittest/kittest.go)): one suite every service runs against its real handler. It checks health and readiness, request IDs, 404s without internal details, and metrics by route pattern. Each check is proven to fail on a handler that breaks its rule.
- **Rules for changing the kit:** [pkg/kit/AGENTS.md](../../pkg/kit/AGENTS.md).

**What callers see differently.** Errors are terror JSON (`{"code":"not_found.account","message":"account not found",…}`) instead of `{"error":"…"}`. A wrong content type or a too-large body is a 400 with its own code (it was 415 or 413), and a wrong method is a 404 (it was 405), because terrors has no codes for those and Typhon's router doesn't answer 405.

## Concepts

- **A service is a function, and shared behaviour wraps it.** A `typhon.Service` is `func(Request) Response`. A `typhon.Filter` is `func(Request, Service) Response`: it can act before and after calling the service it wraps. Stacking filters builds an onion, and each request passes in through every layer and back out.
- **Filter order is behaviour.** Metrics sit outside `ErrorFilter`, so they record the status the caller actually got. Error cleaning sits just inside it, so what it serialises is already safe. Recovery is innermost, so a panic becomes an error like any other.

  One bug came from position alone. The recover filter wraps the router, so the 500 it builds after a panic doesn't carry the matched route, and a panicking route was counted as `unmatched`. The metrics filter now asks the router for the pattern when the response doesn't carry it.
- **Error codes, not status codes.** A terror's code (`not_found.account`) says what went wrong in words both services and people understand. The status (404) is derived from its first part. Calling services can react to the code, and the message is written for the caller.
- **Safe by default.** Services can't leak internals by forgetting a check: anything that isn't a terror is hidden, and every terror is stripped of its stack before it leaves. The full error still reaches the logs, because it's the only record of why a request failed.
- **Stop secrets where they enter.** Rather than hunting for secrets in logs afterwards, the value can't be printed at all. A type with its own `String`, `GoString`, `LogValue` and `MarshalText` makes every print path show `xxxxx`, and reading it takes a deliberate `Reveal()`.
- **Whose fault was it?** A 5xx should mean the service failed. A bad body, an upload cut off part way, or a caller hanging up (499) are the caller's doing, and must not count against the service's error rate.

  The 499 is decided from the request's own context, not from the error, so a `context.Canceled` from inside the service, with the caller still waiting, stays a 500.
- **`errors.As` finds a wrapped terror; `terrors.PrefixMatches` doesn't.** Services wrap errors with context (`create account: %w`). `terrors.PrefixMatches(err, …)` only looks at the outer error, so a wrapped `bad_request` isn't found. The integration tests caught this, because the fake store in the unit tests didn't wrap.
- **Capping a body properly.** `io.LimitReader` stops reading, but `net/http` then reads and throws away up to 256 KB more to reuse the connection. `http.MaxBytesReader` also tells `net/http` to close the connection instead. Both are used: the cap in `kit.Handler`, and a limit in `DecodeJSON` so it's safe on its own.
- **Conformance testing.** When every service gets its rules from shared code, one suite proves each one is actually wired to it. It catches a service that skips `kit.Handler`, or copies part of it by hand, which its own tests wouldn't notice. The suite has its own test, too: a check that never fails gives false confidence.

## How to verify

```bash
make check
```

```bash
go test -v -run 'Conformance|EachCheck' ./pkg/kit/kittest ./cmd/...
```

Both services pass every check. Each `TestEachCheckCatchesItsRuleBeingBroken` subtest logs how its check caught a broken handler, including plain Typhon sending a stack trace.

```bash
make test-integration
```

This needs Docker. It runs the account service against real Postgres, end to end.

```bash
make dev
```

Then send a body with a typo:

```bash
curl -si -X POST localhost:8081/accounts -H 'Content-Type: application/json' -d '{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada","currancy":"GBP"}'
```

You should get `400` with `"code":"bad_request.invalid_json"` and `request body has an unknown field "currancy"`, plus an `X-Request-Id` header. Fix the field name to get `201 Created`. Then:

```bash
curl -si localhost:8081/accounts/c0ffee00-0000-4000-8000-00000000dead
```

That returns `404` with `"code":"not_found.account"`, and no stack trace in the body.
