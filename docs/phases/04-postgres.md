# Phase 4 — Postgres via CloudNativePG, and migrations

## What we built

- **The Postgres operator** (`deploy/cnpg/values.yaml`): `make cnpg` installs [CloudNativePG](https://cloudnative-pg.io) 1.30 into `cnpg-system`, and `make dev` does too. It's installed after monitoring, and services wait for both. Prometheus scrapes the operator, and its Grafana dashboard is loaded. `make cnpg-down` removes only the operator: its CRDs, and so every database, stay.
- **A database per service** (`deploy/charts/service/templates/database.yaml`): `database.enabled: true` in a service's values file makes the shared chart create:
  - a CloudNativePG `Cluster` called `<name>-db`;
  - a `PodMonitor` for its metrics;
  - a `DATABASE_URL` env var, read from the `<name>-db-app` secret the operator generates.
  
  `make chart-check` validates it through `ci/database-values.yaml`, even though no real service turns it on yet.
- **`pkg/db`**:
  - `Open` creates a pgx connection pool from `DATABASE_URL`, with a 5-second connect timeout, and pings it at startup. Its errors never contain the password.
  - `Migrate` applies embedded goose SQL migrations behind a Postgres advisory lock, one transaction per migration.
- **Integration tests** (`*_integration_test.go`, build tag `integration`): they start a throwaway Postgres 18 in Docker with testcontainers. `make test-integration` and a new CI job run them; `make check` stays fast and needs no Docker.
- **Decision:** [ADR-0005](../adr/0005-run-migrations-at-startup-behind-an-advisory-lock.md), on running migrations at startup behind a lock.

**Moved to Phase 5**, with the account service that first uses them: the `accounts` table, the first call to `db.Migrate`, and the `/readyz` database check.

## Concepts

- **Operators.** An operator is a program running in the cluster that manages something complex for you. You write a short `Cluster` object ("one instance, 1 GB"). The operator creates the Postgres pod, its storage, a Service and a secret with a generated password, and keeps them matching what you asked for. If something drifts or breaks, it puts it back.
- **CRDs and deleting them.** A CRD adds a new object type, such as `Cluster`, to Kubernetes. Deleting a CRD deletes every object of that type, so deleting `clusters.postgresql.cnpg.io` would delete every database. The chart keeps its CRDs when it's uninstalled.
- **Secrets as environment variables.** `secretKeyRef` puts one key of a Secret into a container's environment when the pod starts, so the password never appears in git, in a values file or in a ConfigMap. A pod whose secret doesn't exist yet waits, then starts once the operator has created it.
- **One database per service.** Services never share tables. If one needs another's data, it asks that service's API. That lets each service change its own schema without breaking others.
- **Connection pools.** Opening a Postgres connection means a network round trip, authentication and a new server process, which is too slow to do per request. A pool keeps a few connections open and lends them out. `Ping` at startup turns "the database is unreachable" into a clear startup failure instead of a slow first request.
- **Build tags.** `//go:build integration` at the top of a file means it's compiled only with `go test -tags=integration`. That keeps slow, Docker-dependent tests apart from the everyday ones. Lint and vet are told about the tag, so those files are still checked.
- **testcontainers.** A library that starts a real dependency in Docker for a test and removes it afterwards. The tests run against the same Postgres version as the cluster, not a mock.
- **Migrations.** These are numbered SQL files that change the schema in the same order everywhere. goose records which ones it has applied in `goose_db_version`, so applying them again does nothing.
- **Advisory locks.** A named lock in Postgres that only cooperating code uses. A session lock is held until it's released or the connection closes, so a crashed pod can't leave it held. The first pod to start takes it and migrates; the others wait, then find nothing to do.
- **Testing a lock properly.** The first version of the concurrency test passed even with the lock removed. On a brand-new database, goose creates its version table, and pods that lose that race wait one second before retrying. That accidental queue hid the missing lock. The test now starts from a database that has already been migrated once, as real deploys do, and makes the migration slow enough that pods overlap. Without the lock, four of five pods fail.
- **Transactions per migration.** If any statement in a migration fails, Postgres rolls back the whole migration, so the schema is never half-changed.
- **Expand, then contract.** During a rolling update, old and new code run at the same time, so every migration must work for both. Add columns as nullable or with defaults, switch the code over, and only drop what's unused in a later release.

## How to verify

```bash
make check
```

```bash
make chart-check
```

The `ci/database-values.yaml` run should report 6 valid resources, including a `Cluster` and a `PodMonitor`.

```bash
make test-integration
```

This needs Docker running. Look for `pkg/db ... coverage: 90.3% of statements`. To watch the lock test on its own:

```bash
go test -tags=integration -v -run TestMigrateIsSafeWhenPodsStartTogether ./pkg/db
```

On the cluster:

```bash
make dev
```

The Tilt dashboard (localhost:10350) shows `monitoring`, then `cnpg`, then `hello` going green, in that order. Then:

```bash
kubectl --context kind-tiny-bank -n cnpg-system get pods
```

You should see one `cnpg-cloudnative-pg-…` pod running. In Grafana ([localhost:3000](http://localhost:3000)), the *CloudNativePG* dashboard is listed. It shows data once a service has a database (Phase 5).
