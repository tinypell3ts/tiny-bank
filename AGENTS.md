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

## Commits — Conventional Commits

`<type>(<scope>): <summary>`, for example `feat(ledger): add posting`, `docs(adr): record grpc decision`, `chore: add gitignore`.
Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`, `ci`, `build`, `perf`.

## Repo layout

Current:

```
.
├── AGENTS.md            # this file (CLAUDE.md points here)
├── README.md            # overview + quickstart
├── go.mod               # single Go module for the whole monorepo
├── Makefile             # fmt, lint, test, check — CI runs the same targets
├── .golangci.yml        # linter config (golangci-lint v2)
├── .github/             # PR template + CI workflow (workflows/ci.yml)
├── scripts/             # check-branch-name.sh (+ _test.sh), hooks/pre-push
├── cmd/hello/           # demo service: env config, JSON logs, request IDs, health, graceful shutdown
└── docs/
    ├── architecture.md  # evolving architecture diagram
    ├── observability.md # logs, metrics, traces — what exists and why
    ├── adr/             # Architecture Decision Records (NNNN-title.md)
    └── phases/          # one page per phase: what was built and why
```

Planned (added only when a phase needs them): `pkg/` (shared libraries such as `money`, `kit`, `terrors`), `services/<name>/`, `proto/`, `deploy/` (kind, Helm, Tilt).

## Commands

```bash
make             # list targets
make fmt         # format code
make check       # everything CI runs: fmt-check, vet, lint, test (-race -cover), test-scripts
make hooks       # once per clone: enable the pre-push branch-name check
```

Run `make check` before handing work back. CI (`.github/workflows/ci.yml`) runs the same targets on every PR, plus a `branch-name` job.

## Conventions

- **Money is `int64` minor units plus an ISO 4217 currency code.** Never use floats for money, and never mix currencies silently.
- **Config from environment variables** (12-factor). No config files baked into images.
- **Logs are structured JSON to stdout** via `log/slog`. Never log secrets or full card or account numbers.
- **Kubernetes-ready from day one:** expose `/healthz` (liveness) and `/readyz` (readiness), shut down gracefully on `SIGTERM`, and keep no local file state.
- **Errors:** wrap with context (`fmt.Errorf("open account: %w", err)`). Never leak internal errors to API clients.
- **Tests:** table-driven where it fits. Standard library `testing` first.
- **One Go module** for the monorepo, so every service always builds against the latest shared code.

## Engineering principles

- **DRY:** a concern used by two or more services lives in `pkg/*`, never copy-pasted. Rule of two: duplicating once is fine, extract on the second use.
- **KISS:** the simplest thing that works. Standard library before third-party libraries. Readable beats short.
- **YAGNI:** build only what the current phase needs. No speculative config flags, and no interfaces with one implementation unless a test needs them.
- **Tension rule:** YAGNI beats DRY for premature abstraction. Shared platform code (`pkg/kit`) only grows when its first real consumer arrives.
