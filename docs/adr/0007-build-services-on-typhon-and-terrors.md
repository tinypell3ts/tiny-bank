# 7. Build services on Typhon, with terrors for errors

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

The account service (Phase 5) works, but most of its HTTP code isn't about accounts. It checks the content type, limits and decodes the body, maps each error to a status code, hides internal errors, and treats a vanished caller as 499. Three code reviews found bugs in exactly that code, such as 413 becoming 400, and the 499 being decided from the error instead of the request. Every new service would need the same code again.

The plan was to build our own Typhon-style `Service`/`Filter` types and our own typed errors, then move services to gRPC in Phase 6. Two things changed that:

- **The real libraries are available and fit the plan.** [Typhon](https://github.com/monzo/typhon) is an open-source Go RPC framework that wraps `net/http`. Its package doc says it's used for most of Monzo's Go microservices. [terrors](https://github.com/monzo/terrors) is the error package that goes with it, and Typhon already depends on it.
- **Building on them matches the project's goal:** working with a stack close to the one used in production at a modern bank, not a look-alike.

## Decision

**Services are built on Typhon, and return terrors errors.** `pkg/kit` stays our shared foundation, but becomes a thin layer around Typhon instead of our own server code.

- **A service is a `typhon.Service`:** `func(typhon.Request) typhon.Response`. Handlers decode with `kit.DecodeJSON` (Typhon's `req.Decode` doesn't check the content type, size, unknown fields or trailing data), reply with `req.Response(body)`, and fail with a terrors error, for example `terrors.NotFound("account", "account not found", nil)`.
- **Behaviour every service needs is a `typhon.Filter`:** `func(Request, Service) Response`, applied in one place by `pkg/kit`.
  - **Typhon's own:** `ErrorFilter`, which turns errors into responses with the right status.
  - **Ours:** request IDs, access logs, RED metrics (from `pkg/obs`, labelled by the route pattern the router matched), recovering from panics, and cleaning errors. `ErrorFilter` on its own sends a terror's stack trace, with file paths from the server, to the caller, and turns any non-terror error's text into the message. Our filter, just inside it, sends only the code, message and params, and logs everything else.
- **`pkg/kit` keeps** `Main`, config, the JSON logger, and running and stopping the server gracefully. `kit.Serve` stays on `net/http`'s `http.Server`, serving `typhon.HttpHandler`, rather than switching to `typhon.Listen`: it already cancels requests still running at the shutdown deadline, which `typhon.Listen` doesn't. Every service also gets `/healthz` (`kit.Health`) and `/metrics`, and writes its own `/readyz`.
- **Services call each other with Typhon clients** over HTTP. In Phase 6, the request and response shapes become protobuf messages in `proto/`, sent as protobuf bodies (Typhon's `EncodeAsProtobuf`), with `buf` linting them and catching breaking changes. This replaces the earlier plan to use gRPC.
- **Errors travel between services.** Typhon's `ErrorFilter` sends a terror's code, message and params to the calling service, so the caller can react to, say, `not_found`. A terror's params must therefore never hold secrets or personal details. The edge gateway (Phase 12) turns them into responses that are safe for customers.

### Alternatives considered

- **Our own `Service`/`Filter` and typed errors** (the original plan). They'd be smaller, and fit our needs exactly, including a caller-safe message kept apart from the hidden cause. But we'd maintain them ourselves, and it would mean working with a copy of the real thing instead of the libraries themselves.
- **Plain `net/http` with handlers that each do their own encoding and errors.** This is what Phase 5 did. It's simple for one service, but it repeats the same fiddly code in each, and that's where the bugs were.
- **gRPC.** It's the most common choice for service-to-service calls, with strong code generation, streaming, and built-in Kubernetes health probes. But it's a different programming model from Typhon's, which is ordinary HTTP with filters. Doing both would split the project's attention. gRPC remains a good alternative to be able to compare against.
- **terrors on its own, without Typhon.** We'd get the error codes, but still write and maintain our own filter chain and error-to-response mapping, which Typhon already provides.

## Consequences

- Handlers shrink to the domain logic: decode, call the store, return a response or a terror. The fiddly HTTP code lives in one place.
- **Older dependencies come with it.** Typhon targets Go 1.19 and brings `github.com/golang/protobuf` v1.5 (the older protobuf API, built on the newer one), `github.com/monzo/slog`, and `golang.org/x/net` v0.7.0. That `x/net` version is missing HTTP/2 security fixes, so `go.mod` requires a current `x/net`, which overrides it. Typhon's `slog` sits alongside the standard `log/slog` that our services use.
- terrors errors record a stack trace when created. That helps when debugging a 500, but has a small cost per error. It's stripped before the error reaches a caller, and expected answers that happen often, such as an account not being found, are built without one.
- Error details are visible to calling services by design, which makes "no secrets or personal data in terror params" a rule (AGENTS.md). Customer-facing responses are cleaned up at the edge.
- Typhon is less widely known than gRPC, and its releases are infrequent (v1.1.10, September 2024). If it stopped being maintained, `pkg/kit` is the one place that would need to change, because services only see `typhon.Service` and terrors.
- terrors has no tagged releases, so `go.mod` pins a commit (a Go pseudo-version). Upgrading it is a deliberate change, like any other dependency.
