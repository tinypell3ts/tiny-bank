# pkg/kit/AGENTS.md

Rules for changing `pkg/kit`, the foundation every service is built on. The root [AGENTS.md](../../AGENTS.md) still applies. Why the kit is built on Typhon and terrors is in [ADR-0007](../../docs/adr/0007-build-services-on-typhon-and-terrors.md).

A change here reaches every service at once, so it gets more care than service code.

## Layout

```
pkg/kit/
├── kit.go        # Main (signals, exit code) and NewLogger
├── config.go     # Config and LoadConfig: PORT, LOG_LEVEL, SHUTDOWN_TIMEOUT
├── secret.go     # Secret: prints as xxxxx everywhere; Reveal() is the only way in
├── serve.go      # Serve: http.Server with graceful shutdown, cancelling requests left at the deadline
├── handler.go    # Handler: the filter chain, /metrics, the body cap; Health
├── decode.go     # DecodeJSON: strict JSON bodies, every failure a coded 400
├── requestid.go  # RequestIDHeader, RequestIDFrom, which caller IDs are safe to reuse
└── kittest/      # Conformance: the checks every service's handler must pass
```

## The filter chain

`Handler` wraps a service's router in filters, outermost first. The order matters, so keep it, and change it only with a test showing why:

1. **Request ID**, so everything inside can log it.
2. **Access log**, one line per request.
3. **Metrics**, outside `ErrorFilter` so they see the final status. The route label comes from the routed request, or from `router.Pattern` when a response doesn't carry it (the 500 after a panic).
4. **Typhon's `ErrorFilter`**, which turns a returned error into a response with the right status.
5. **Clean errors**, just inside `ErrorFilter`, so the error it serialises is already safe: only a terror's code, message and params; stack and message chain stripped; anything else logged and replaced with a generic 500; 499 when the caller has gone.
6. **Recover**, innermost, so a panic becomes an error the filters above handle like any other.

`/metrics` sits beside the chain, so scrapes aren't counted as traffic. Every body is capped at `MaxBodyBytes` before Typhon sees the request.

## Rules

- **Grow only for a real consumer.** Add to the kit when a service needs it now, not because one might (the root tension rule). Typhon clients, for example, arrive with Phase 6, the first service-to-service call.
- **Nothing service-specific.** The kit never imports `services/` or `cmd/`, and never knows a route, table or field name. If only one service needs it, it stays in that service.
- **Callers see only what's meant for them.** Anything that reaches a caller is a terror's code, message and params, never a stack, a message chain, a file path or a non-terror error's text. `TestErrorsReachTheCallerSafely` and kittest's "unknown routes" check guard this: keep both passing.
- **A caller's mistake is a 4xx, never a 5xx.** A bad body, a cut-off upload or a caller hanging up must not count against the service's error rate, or alerts fire for problems that aren't ours.
- **Find a terror with `errors.As`.** `terrors.PrefixMatches(err, …)` and `terrors.Is` don't look inside `fmt.Errorf("…: %w")`, and services wrap.
- **Log the full error, never a secret.** When a request fails, the kit's log line is the only record of why, so it holds the whole error. Secrets stay out because they're `Secret` values, which never print.
- **A new rule for every service gets a kittest check.** Add the check to `kittest`, and a handler that breaks the rule to `TestEachCheckCatchesItsRuleBeingBroken`, so the check is proven to fail. Then every service's `TestConformance` enforces it.
- **Prove each guard works.** After adding a check or a fix, put the bug back and watch a test fail. A test that passes either way protects nothing.
- **Changing the kit's API means updating the services and the docs together:** every caller in `cmd/` and `services/`, the root AGENTS.md conventions, and [observability.md](../../docs/observability.md) if logs or metrics change.
- **Typhon and terrors are bumped on purpose.** Typhon pulls in an old `golang.org/x/net`, which `go.mod` overrides with a current one for its security fixes. Keep that override when bumping, and run `make check` and `make test-integration`.
