# Phase 0 — Go foundations + CI

## What we built

- **Repo conventions:** `AGENTS.md` (with `CLAUDE.md` pointing to it), a plain-language PR template, ADRs, and the docs skeleton.
- **CI** (`.github/workflows/ci.yml`): on every PR it checks gofmt, `go vet`, golangci-lint, `go test -race -cover`, and the branch name. A `Makefile` runs the same checks locally (`make check`).
- **Branch naming:** Conventional Branch, enforced in CI and by an optional `pre-push` hook (`make hooks`), both through one script.
- **`cmd/hello`:** the smallest possible service, carrying the foundations every service will share:
  - config from env (`PORT`, `LOG_LEVEL`, `SHUTDOWN_TIMEOUT`)
  - structured JSON logs on stdout
  - request IDs, reused from `X-Request-Id` or generated, echoed back and logged
  - `/healthz` (liveness) and `/readyz` (readiness)
  - graceful shutdown on `SIGTERM`

## Concepts

- **Modules and packages.** One `go.mod` for the whole monorepo. `cmd/<name>` holds runnable programs (`package main`).
- **`context.Context`** carries cancellation and request-scoped values. `signal.NotifyContext` turns SIGTERM or Ctrl-C into a cancelled context, and that one signal drives the whole shutdown.
- **Goroutines and channels.** The server runs in a goroutine. `main` waits on a `select` for either a server error or cancellation. The error channel is buffered so the goroutine can always exit, with no leak.
- **Graceful shutdown:** mark not-ready, stop accepting connections, let in-flight requests finish within a deadline. Kubernetes sends SIGTERM, waits `terminationGracePeriodSeconds`, then SIGKILL. Phase 1 lines these timeouts up.
- **Liveness vs readiness.** A failing liveness check means Kubernetes restarts the pod. A failing readiness check means it stops sending traffic. Liveness must never check dependencies like the database, or a database blip restarts every pod at once.
- **Middleware** is a function that wraps an `http.Handler`. Request ID runs outermost so the access log can include it. (In Phase 5.5 this becomes the Typhon-style `Filter` in `pkg/kit`.)
- **Untrusted input in logs.** Incoming request IDs are length- and character-checked, so a caller can't forge log lines with newlines.
- **Testable `main`.** `main` only wires the OS (signals, `os.Getenv`, stdout) into `run`. Tests call `run`, `serve` and `loadConfig` with fakes.
- **Testing:** table-driven tests, `httptest` for handlers without a network, and a real listener on `127.0.0.1:0` to test shutdown. `-race` catches data races, which is why `ready` is an `atomic.Bool`.
- **CI basics:** workflows, jobs, steps, read-only `permissions`, `concurrency` to cancel stale runs, and passing `github.head_ref` through `env` to avoid script injection.

## How to verify

```bash
make check
```

```bash
go run ./cmd/hello
```

In another terminal:

```bash
curl -i -H 'X-Request-Id: demo-1' localhost:8080/readyz
```

Then press Ctrl-C in the first terminal. You should see `shutting down` and `server stopped` log lines, and the process exits with status 0.
