# 5. Run database migrations at service startup, behind a Postgres advisory lock

- **Status:** Accepted
- **Date:** 2026-11-13

## Context

Each service owns a Postgres database ([ADR-0002](0002-run-everything-in-docker-on-kubernetes.md), Phase 4). Its tables change over time, and every environment must apply the same changes in the same order. Those changes are migrations: numbered SQL files kept in git with the code.

On Kubernetes, what makes this hard is *when* and *where* migrations run:

- **Several pods start at once.** A deploy or a scale-up starts more than one replica. If each runs the migrations, two can apply the same change together and fail, or leave the schema half-changed.
- **Old and new code overlap.** A rolling update keeps old pods serving while new ones start. For a while, old code runs against the new schema.
- **The database may not exist yet.** On the first install, the shared chart creates the CloudNativePG `Cluster` in the same release as the service, so it isn't ready until about a minute later.
- **Two ways to deploy.** `make deploy` uses `helm upgrade --install`. Tilt renders the chart with `helm template` and applies the result itself. So Helm hooks don't work as hooks under Tilt: a hook Job is applied alongside everything else, with no "run this first" ordering.
- **A failed migration must be visible and stop the rollout**, without breaking pods that are already serving.

## Decision

**Each service runs its own migrations when it starts, before it reports ready**, by calling `db.Migrate` from [`pkg/db`](../../pkg/db/migrate.go).

- **Tool: [goose](https://github.com/pressly/goose).** Migrations are plain SQL files named `<version>_<name>.sql`, like `00001_create_accounts.sql`. Each file has an `-- +goose Up` section and a `-- +goose Down` section. They're embedded in the service binary with `//go:embed`, so the image always carries exactly the migrations its code expects. goose records what's been applied in a `goose_db_version` table.
- **Lock: a Postgres session advisory lock**, held for the whole run (goose's `SessionLocker`). An advisory lock is a named lock in Postgres that only our code agrees to use. The first pod to take it migrates. The others check every second, for up to 5 minutes, then find nothing left to do and carry on. `TestMigrateIsSafeWhenPodsStartTogether` starts five pods at once: with the lock, the migration runs once; without it, four of them fail.
- **One transaction per migration.** If any statement fails, the whole migration rolls back, and nothing after it runs. `TestMigrateRollsBackAFailingMigration` checks this. The pod then exits with the error, so it never becomes ready, and Kubernetes stops the rollout while old pods keep serving.
- **Expand, then contract.** Because old and new code overlap, every migration must work with the code that's currently running:
  - Add before you use: new columns are nullable or have defaults.
  - Remove only after nothing uses it. Renaming a column takes three releases: add the new column, switch the code over, then drop the old one.
  - Never edit a migration that has been applied anywhere. Fix forward with a new one.
- **Migrations only move forward when deployed.** The `Down` sections exist for local development and tests, but services never roll back automatically. To undo a change, write a new migration.

### Alternatives considered

- **A Helm `pre-install`/`pre-upgrade` hook Job.** This is the classic answer: one Job migrates before the new pods start. It doesn't fit here, for three reasons:
  - On first install the hook runs before the release's `Cluster` exists, so there's no database to migrate.
  - Tilt doesn't treat Helm hooks as hooks: the Job would start at the same time as the new pods, not before them.
  - It adds a second workload, with its own image, permissions and failure handling.
- **A separate migration command or CI step** (`goose up` from a pipeline). It needs network access to every database from outside the cluster, and it can drift out of step with the code that's being deployed.
- **An init container in each pod.** Like running at startup, but the init container runs once per pod anyway. It still needs the same lock, and adds a second container to configure. Running in the process is simpler, and it uses the same connection pool.
- **Running migrations at startup without a lock.** Fine with exactly one replica, but unsafe the moment there are two. The test above shows it failing.
- **Other tools** (golang-migrate, Atlas, a framework's auto-migrate). golang-migrate is similar to goose. We picked goose for its `Provider` API and built-in session locker. Atlas computes schema changes for you, which is powerful but hides the SQL we want reviewed. Auto-migrating from models can't express expand/contract safely.

## Consequences

- A service applies its own migrations with one call at startup, and a new replica needs no extra setup.
- Every pod pays a small startup cost to check for pending migrations, a few milliseconds when there's nothing to do. While one pod migrates, the others wait, up to 5 minutes.
- A long migration delays pods from becoming ready by that long. Heavy changes, such as backfilling millions of rows, belong in a background job, not a migration.
- Expand/contract makes some changes take several releases. That's the price of zero-downtime deploys.
- The service's database user needs permission to change the schema. CloudNativePG's `app` user owns its database, so it can. Splitting migration rights from runtime rights is deferred until there's a reason.
- The first real migration, the `accounts` table, arrives with the account service in Phase 5, which is also where `db.Migrate` is first called and the `/readyz` database check is added.
