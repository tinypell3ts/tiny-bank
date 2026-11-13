# AGENTS.md

Guidance for AI coding agents (and humans) working in this repo. Nested `AGENTS.md` files in subfolders add local rules. The nearest one wins for its folder.

## Project purpose

tiny-bank is a core banking platform made of Go microservices, with Postgres for storage and Kafka for events. Code should be clear and easy to read. Correctness of money handling matters more than features.

The target runtime is **Docker on Kubernetes**: a local kind cluster, services deployed with Helm, the dev loop run by Tilt, Postgres via CloudNativePG, and Kafka via Strimzi. See [docs/architecture.md](docs/architecture.md).

## Working agreement

- Work in **small phases**. Each phase is one PR, split into several small commits. The plan lives in [docs/phases/](docs/phases/).
- **Agents never run git write commands.** Do not create branches, commit, push, tag or open PRs. Write the files, explain them, and suggest a commit message. The human runs git.
- After each step: suggest a Conventional Commit message, explain the concepts and why, give a verify command, then **stop for questions**.
- **A PR is not done without tests and docs.** New code gets tests. Changed behaviour gets updated docs (README, `docs/`, ADRs, and `AGENTS.md` if layout or commands change).

## Branches — Conventional Branch

Format `<type>/<description>`, trunk is `main`.

- Types: `feature/` (alias `feat/`), `bugfix/` (alias `fix/`), `hotfix/`, `release/`, `chore/`.
- Lowercase `a-z`, `0-9` and hyphens only. Dots are allowed only in release versions (`release/v1.2.0`). No leading, trailing or repeated hyphens or dots.
- Optional ticket ref: `feature/issue-12-ledger-postings`.
- Examples: `feature/phase-0-foundations`, `chore/bump-go`.
- **Never use vendor or agent prefixes** such as `claude/`, `copilot/` or `codex/`.
- **This is deliberately stricter than the published spec** ([conventionalbranch.org](https://conventionalbranch.org)), which allows dots in any branch, AI-agent types like `claude/`, and any description after `release/`. We allow dots only in versions, ban agent types, and require `release/` to be a version. The rule lives in `scripts/check-branch-name.sh`, which `scripts/check-branch-name_test.sh` tests.

## Commits — Conventional Commits

`<type>(<scope>): <summary>`, for example `feat(ledger): add posting`, `docs(adr): record typhon decision`, `chore: add gitignore`.
Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`, `ci`, `build`, `perf`.

## Repo layout

Current:

```
.
├── AGENTS.md            # this file (CLAUDE.md points here)
├── README.md            # overview + quickstart
├── go.mod               # single Go module for the whole monorepo
├── Makefile             # fmt, lint, test, check, docker-* — CI runs the same targets
├── Dockerfile           # one multi-stage, distroless image recipe for every service (PKG build arg)
├── Tiltfile             # dev loop: build + deploy + port-forward every service, redeploy on save
├── .golangci.yml        # linter config (golangci-lint v2)
├── .github/             # PR template + CI workflow (workflows/ci.yml)
├── scripts/             # check-branch-name.sh (+ _test.sh), docker-smoke.sh, deploy.sh, hooks/pre-push
├── cmd/hello/           # demo service: the smallest service built on pkg/kit
├── cmd/account/         # account service binary: config, startup (connect, migrate), HTTP server, probes
├── services/account/    # account service code: domain, Postgres store, HTTP API, migrations/*.sql
├── pkg/kit/             # every service's foundation: Main, config, JSON logger, Serve (graceful shutdown), RequestID, AccessLog, WriteJSON, Healthz
├── pkg/obs/             # shared observability: Prometheus registry, /metrics handler, HTTP RED middleware, StatusRecorder
├── pkg/db/              # Postgres: Open a pgx pool from DATABASE_URL, Migrate with goose behind an advisory lock
│   └── dbtest/          # StartPostgres for integration tests (testcontainers)
├── pkg/money/           # Money (int64 minor units + Currency): Add/Sub that never mix currencies or overflow
├── deploy/
│   ├── kind/            # local kind cluster config
│   ├── charts/service/  # ONE generic Helm chart used by every service
│   ├── cnpg/            # CloudNativePG operator values (Postgres for services)
│   ├── monitoring/      # kube-prometheus-stack values + dashboards/*.json (Grafana) for the local cluster
│   └── values/          # one values file per service (hello.yaml, …)
└── docs/
    ├── architecture.md  # evolving architecture diagram
    ├── observability.md # logs, metrics, traces — what exists and why
    ├── adr/             # Architecture Decision Records (NNNN-title.md)
    └── phases/          # one page per phase: what was built and why
```

Each service is a binary in `cmd/<name>/` (wiring only) plus its code in `services/<name>/`. Planned (added only when a phase needs them): `pkg/kit` on [Typhon](https://github.com/monzo/typhon) with [terrors](https://github.com/monzo/terrors) errors ([ADR-0007](docs/adr/0007-build-services-on-typhon-and-terrors.md)), and `proto/`.

Nested rules: [deploy/AGENTS.md](deploy/AGENTS.md) for the cluster, chart and dev loop.

## Commands

```bash
make             # list targets
make fmt         # format code
make check       # everything CI runs: fmt-check, vet, lint, test (-race -cover), test-scripts
make test-integration # integration tests against real Postgres in Docker (CI runs these too)
make hooks       # once per clone: enable the pre-push branch-name check
make docker-smoke             # build the hello image and smoke-test it
make docker-smoke SERVICE=x   # same for cmd/x
make cluster-up               # create the local kind cluster "tiny-bank" (no-op if it exists)
make image-load               # build IMAGE and load it into the cluster
make cluster-down             # delete the cluster
make deploy                   # build SERVICE (default hello) and helm-install exactly that build (needs make monitoring first)
make undeploy                 # helm-uninstall SERVICE (deletes its database and data too)
make chart-check              # helm lint + kubeconform (incl. CRD schemas) for every deploy/values/*.yaml and chart ci/*.yaml
make monitoring               # install or upgrade Prometheus + Grafana in namespace "monitoring" (make dev does this)
make monitoring-down          # remove the monitoring stack
make cnpg                     # install or upgrade the CloudNativePG operator in "cnpg-system" (make dev does this)
make cnpg-down                # remove the operator (CRDs and databases stay)
make dashboards               # reload deploy/monitoring/dashboards/*.json into Grafana (Tilt does this on save)
make dashboards-check         # validate the dashboard JSON (CI runs this)
make dev                      # day-to-day: Tilt builds, deploys and port-forwards everything, redeploys on save
make dev-down                 # remove what Tilt deployed, INCLUDING every service database and its data (cluster, monitoring, CloudNativePG stay)
```

Use `make dev` for everyday work, and `make deploy` for a one-off deploy without Tilt. Don't mix them for the same service: both create the same resources. A new service needs one `go_service(...)` line in the `Tiltfile`, with its own localhost port. Ports 8080 (hello), 8081 (account), 3000 (Grafana) and 9090 (Prometheus) are taken.

`make deploy` tags each build `build-<hash of the image's layers>`, so unchanged code means no restart and changed code means a rollout. Never deploy a fixed tag like `:dev`, because Kubernetes can't tell it changed.

Cluster commands use the kubectl context `kind-tiny-bank`. Always pass `--context kind-tiny-bank` in scripts and docs, so nothing ever runs against another cluster by accident.

Run `make check` before handing work back. CI (`.github/workflows/ci.yml`) runs the same targets on every PR, plus a `branch-name` job.

## Conventions

- **Money is `int64` minor units plus an ISO 4217 currency code**, always as `money.Money` from `pkg/money` ([ADR-0004](docs/adr/0004-represent-money-as-int64-minor-units.md)). Never use floats for money, never mix currencies silently, and always handle the error from `Add`/`Sub`. Never discard it with `_`: lint doesn't catch that, so reviewers must.
- **Every service is built on `pkg/kit` and [Typhon](https://github.com/monzo/typhon):** `kit.Main`, `kit.LoadConfig` (embed `kit.Config` for extra settings), `kit.NewLogger` and `kit.Serve`. Routes are a `typhon.Router` of `typhon.Service` handlers, and `kit.Handler` wraps it in the shared filters: request IDs, access logs, metrics, error cleaning and panic recovery. Handlers fail by returning a terrors error. A service only writes its own routes, readiness check and startup. Never copy the filters into a service. (The account service moves onto Typhon later in Phase 5.5.)
- **Config from environment variables** (12-factor). No config files baked into images.
- **Logs are structured JSON to stdout** via `log/slog`. Never log secrets or full card or account numbers.
- **Metrics come from `pkg/obs`.** Every HTTP service serves `GET /metrics` and wraps its mux in `obs.HTTPMetrics.Middleware`. Metric labels must have a small, fixed set of values: use route patterns (`/accounts/{id}`), never raw paths, IDs, emails or amounts.
- **Kubernetes-ready from day one:** expose `/healthz` (liveness) and `/readyz` (readiness), shut down gracefully on `SIGTERM`, and keep no local file state.
- **Errors:** wrap with context (`fmt.Errorf("open account: %w", err)`). Never leak internal errors to API clients. Never put secrets, tokens or personal details in an error: when a request fails, `pkg/kit` logs the full error, because it's the only record of why.
- **terrors travel between services.** Typhon sends an error's code, message and params to the calling service ([ADR-0007](docs/adr/0007-build-services-on-typhon-and-terrors.md)). Never put secrets, passwords, tokens or personal details (names, emails, full account numbers) in a terror's message or params.
- **Tests:** table-driven where it fits. Standard library `testing` first. Tests that need a real dependency (Postgres) go in `*_integration_test.go` files tagged `//go:build integration`, and start it with testcontainers. `make check` skips them; `make test-integration` and CI run them.
- **Databases:** connect with `db.Open` from `pkg/db`, using `DATABASE_URL` from the environment. Never build connection strings by hand, and never log `DATABASE_URL` itself (errors from `db.Open` are safe to log).
- **Migrations** are goose SQL files (`<version>_<name>.sql`) embedded in the service and applied at startup with `db.Migrate`, before the service reports ready ([ADR-0005](docs/adr/0005-run-migrations-at-startup-behind-an-advisory-lock.md)). Every migration must work with the code that is already running (expand, then contract). Never edit a migration that has been applied anywhere; fix forward with a new one.
- **One Go module** for the monorepo, so every service always builds against the latest shared code.
- **Bump Go in three places together:** `go.mod`, `GO_VERSION` in the `Dockerfile`, and the golangci-lint version in CI, which must be built with a Go version at least as new.
- **Images** are distroless and run as non-root. Never add a shell or package manager to the runtime stage.

## Engineering principles

- **DRY:** a concern used by two or more services lives in `pkg/*`, never copy-pasted. Rule of two: duplicating once is fine, extract on the second use.
- **KISS:** the simplest thing that works. Standard library before third-party libraries. Readable beats short.
- **YAGNI:** build only what the current phase needs. No speculative config flags, and no interfaces with one implementation unless a test needs them.
- **Tension rule:** YAGNI beats DRY for premature abstraction. Shared platform code (`pkg/kit`) only grows when its first real consumer arrives.
