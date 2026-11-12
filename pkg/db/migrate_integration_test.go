//go:build integration

package db

import (
	"context"
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	goodMigrations   = os.DirFS("testdata/migrations")
	brokenMigrations = os.DirFS("testdata/broken")
)

func openTestDB(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrateAppliesInOrderThenNothing(t *testing.T) {
	pool := openTestDB(t, startPostgres(t))
	ctx := t.Context()

	applied, err := Migrate(ctx, pool, goodMigrations)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := []string{"00001_create_widgets.sql", "00002_add_widget_colour.sql"}
	if !slices.Equal(applied, want) {
		t.Fatalf("applied %v, want %v", applied, want)
	}

	// Both migrations took effect: the column from 00002 has its default.
	var colour string
	if err := pool.QueryRow(ctx, "INSERT INTO widgets (name) VALUES ('w') RETURNING colour").Scan(&colour); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if colour != "blue" {
		t.Errorf("colour = %q, want the default from 00002", colour)
	}

	// Every startup calls Migrate. The second time there's nothing to do.
	again, err := Migrate(ctx, pool, goodMigrations)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Migrate applied %v, %v; want nothing", again, err)
	}
}

func TestMigrateIsSafeWhenPodsStartTogether(t *testing.T) {
	dsn := startPostgres(t)
	const pods = 5

	// Each "pod" has its own pool, like separate processes.
	pools := make([]*pgxpool.Pool, pods)
	for i := range pools {
		pools[i] = openTestDB(t, dsn)
	}

	// Start from a database that's already been migrated once, as on every
	// deploy after the first: only 00001 is applied, so goose's version table
	// exists. (On a brand-new database goose retries creating that table a
	// second apart, which happens to queue the pods up and would hide a
	// missing lock.)
	first, err := fs.ReadFile(goodMigrations, "00001_create_widgets.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(t.Context(), pools[0], fstest.MapFS{
		"00001_create_widgets.sql": {Data: first},
	}); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	// Now all pods start migrating at the same moment. 00002 takes half a
	// second, so without the lock they would all run it at once.
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		applied = make([][]string, pods)
		errs    = make([]error, pods)
	)
	for i := range pods {
		wg.Go(func() {
			<-start
			applied[i], errs[i] = Migrate(t.Context(), pools[i], goodMigrations)
		})
	}
	close(start)
	wg.Wait()

	total := 0
	for i := range pods {
		if errs[i] != nil {
			t.Errorf("pod %d: %v", i, errs[i])
		}
		total += len(applied[i])
	}
	// The lock means 00002 ran exactly once, in whichever pod got the lock first.
	if total != 1 {
		t.Errorf("applied %d migrations across %d pods, want 1: %v", total, pods, applied)
	}
	var versions int
	if err := pools[0].QueryRow(t.Context(),
		"SELECT count(*) FROM goose_db_version WHERE version_id > 0").Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versions != 2 {
		t.Errorf("goose_db_version has %d migrations recorded, want 2", versions)
	}
}

func TestMigrateRollsBackAFailingMigration(t *testing.T) {
	pool := openTestDB(t, startPostgres(t))
	ctx := t.Context()

	applied, err := Migrate(ctx, pool, brokenMigrations)
	if err == nil {
		t.Fatal("Migrate succeeded, want the error from 00002_half_broken.sql")
	}
	if !slices.Equal(applied, []string{"00001_create_widgets.sql"}) {
		t.Errorf("applied %v, want only the migration before the broken one", applied)
	}

	// 00002's first statement succeeded before its second failed. It must have
	// been rolled back with the rest of that migration.
	var hasSize bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'widgets' AND column_name = 'size')`).Scan(&hasSize); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if hasSize {
		t.Error("column widgets.size exists: the failed migration was half-applied")
	}
}

func TestMigrateNeedsAnEmptyOrMigratedDatabase(t *testing.T) {
	// A table goose doesn't know about doesn't stop it: Migrate only tracks its
	// own versions. This documents that migrations must create everything.
	pool := openTestDB(t, startPostgres(t))
	if _, err := pool.Exec(t.Context(), "CREATE TABLE unrelated (id int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(t.Context(), pool, goodMigrations); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}
