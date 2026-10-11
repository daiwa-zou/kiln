package store

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lateMigrationSet builds migrations that each create a table named after the
// version, so a test can tell from the catalog which ones actually ran.
func lateMigrationSet(versions ...int) []migration {
	out := make([]migration, len(versions))
	for i, v := range versions {
		out[i] = migration{
			version: v,
			name:    "late",
			body:    fmt.Sprintf("CREATE TABLE late_%d (id int)", v),
		}
	}
	return out
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	return exists
}

// TestMigrateBackfillsLateMigration is the out-of-order landing: version 3 is
// applied to a database before version 2 exists in the binary, then version 2
// arrives. Comparing against max(version) skipped it forever.
func TestMigrateBackfillsLateMigration(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	// Capture the operator-facing log line for the back-fill.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	applied, err := migrateWith(ctx, pool, lateMigrationSet(1, 3))
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if want := []int{1, 3}; !slices.Equal(applied, want) {
		t.Fatalf("first migrate applied %v, want %v", applied, want)
	}
	if tableExists(ctx, t, pool, "late_2") {
		t.Fatal("late_2 exists before its migration was embedded")
	}
	if strings.Contains(logs.String(), "below") {
		t.Errorf("in-order run logged a back-fill: %s", logs.String())
	}

	// The binary now embeds 1, 2, 3. Version 2 is below the applied maximum.
	applied, err = migrateWith(ctx, pool, lateMigrationSet(1, 2, 3))
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if want := []int{2}; !slices.Equal(applied, want) {
		t.Errorf("second migrate applied %v, want %v (the back-filled version only)", applied, want)
	}
	if !tableExists(ctx, t, pool, "late_2") {
		t.Error("late_2 was not created; the late migration was skipped")
	}
	if got := logs.String(); !strings.Contains(got, "version=2") || !strings.Contains(got, "highest_applied=3") {
		t.Errorf("back-fill was not logged with its version and the highest applied; log = %q", got)
	}

	versions, err := AppliedVersions(ctx, pool)
	if err != nil {
		t.Fatalf("AppliedVersions: %v", err)
	}
	if want := []int{1, 2, 3}; !slices.Equal(versions, want) {
		t.Errorf("applied versions = %v, want %v", versions, want)
	}

	// And it settles: nothing is owed on the next run.
	applied, err = migrateWith(ctx, pool, lateMigrationSet(1, 2, 3))
	if err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("third migrate applied %v, want nothing", applied)
	}
}

// A back-filled migration that fails must leave no trace: its own transaction
// rolls back, and the next run still owes it.
func TestMigrateBackfillFailureIsNotRecorded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := migrateWith(ctx, pool, lateMigrationSet(1, 3)); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	bad := lateMigrationSet(1, 2, 3)
	bad[1].body = "CREATE TABLE late_2 (id int); SELECT * FROM does_not_exist"
	_, err := migrateWith(ctx, pool, bad)
	if err == nil || !strings.Contains(err.Error(), "apply migration 002_late") {
		t.Fatalf("migrate = %v, want an apply error for 002_late", err)
	}
	if tableExists(ctx, t, pool, "late_2") {
		t.Error("late_2 survived a failed migration; the transaction did not roll back")
	}

	applied, err := migrateWith(ctx, pool, lateMigrationSet(1, 2, 3))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if want := []int{2}; !slices.Equal(applied, want) {
		t.Errorf("retry applied %v, want %v", applied, want)
	}
}

// holdMigrateLock takes the migration lock the way a migration in progress
// does -- inside an open transaction on its own connection -- and returns the
// function that ends that transaction and so releases it.
func holdMigrateLock(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("holder lock: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

func resetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

type migrateResult struct {
	applied []int
	err     error
}

// TestMigrateWaitsForLockHolder: a migration owed while another run holds the
// lock waits for it rather than racing it, then applies.
func TestMigrateWaitsForLockHolder(t *testing.T) {
	pool := testPool(t)
	resetSchema(t, pool)
	release := holdMigrateLock(t, pool)

	done := make(chan migrateResult, 1)
	go func() {
		applied, err := migrateWith(context.Background(), pool, lateMigrationSet(1))
		done <- migrateResult{applied, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("migrate returned (%v, %v) while another run held the lock", r.applied, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	if tableExists(context.Background(), t, pool, "late_1") {
		t.Fatal("late_1 was created while another run held the migration lock")
	}

	release()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("migrate after release: %v", r.err)
		}
		if want := []int{1}; !slices.Equal(r.applied, want) {
			t.Errorf("applied %v, want %v", r.applied, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("migrate did not proceed after the lock holder released")
	}
}

// TestMigrateLockTimeoutMessage: a run stuck behind a hung migration gets a
// diagnosis rather than an indefinite silent block.
func TestMigrateLockTimeoutMessage(t *testing.T) {
	pool := testPool(t)
	resetSchema(t, pool)
	prev := migrateLockTimeout
	migrateLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { migrateLockTimeout = prev })
	holdMigrateLock(t, pool)

	// The caller's own deadline is far above the lock timeout, so only the
	// lock timeout can produce the diagnosis.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := migrateWith(ctx, pool, lateMigrationSet(1))
	if err == nil {
		t.Fatal("migrate succeeded while another run held the lock")
	}
	if !strings.Contains(err.Error(), "another migration may be hung") {
		t.Errorf("error = %v, want the hung-migration diagnosis", err)
	}
	if ctx.Err() != nil {
		t.Errorf("migrate ran into the caller's deadline instead of the lock timeout: %v", err)
	}
}

// TestMigrateFastPathTakesNoLock: an up-to-date database is reported as such
// without queueing behind a migration that is still running elsewhere. Every
// `kiln admin migrate` in a rollout would otherwise wait on the slowest one.
func TestMigrateFastPathTakesNoLock(t *testing.T) {
	pool := testPool(t)
	freshSchema(t, pool)
	holdMigrateLock(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applied, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("Migrate with nothing owed: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied %v, want nothing", applied)
	}
}

// TestMigrateRestoresDDLLockTimeout: the lock timeout bounds only the wait for
// the migration lock. A migration's DDL keeps Postgres's unbounded default, as
// before, so a long table lock queue is waited out rather than failed.
func TestMigrateRestoresDDLLockTimeout(t *testing.T) {
	pool := testPool(t)
	resetSchema(t, pool)
	set := []migration{{
		version: 1, name: "probe",
		body: `CREATE TABLE lock_timeout_probe AS SELECT current_setting('lock_timeout') AS v`,
	}}
	if _, err := migrateWith(context.Background(), pool, set); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var got string
	if err := pool.QueryRow(context.Background(), `SELECT v FROM lock_timeout_probe`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "0" {
		t.Errorf("migration body ran with lock_timeout = %q, want 0", got)
	}
}

// TestMigrateLeavesNoSessionLocks is the pooler proof. Through a transaction-mode
// pooler a session advisory lock and its unlock can land on different server
// connections, leaving the lock held by a connection nobody owns. Only a direct
// connection can see that, so this runs only in the pooled CI leg, where
// KILN_TEST_DATABASE_URL is the pooler and KILN_TEST_DIRECT_DATABASE_URL is not.
func TestMigrateLeavesNoSessionLocks(t *testing.T) {
	direct := os.Getenv("KILN_TEST_DIRECT_DATABASE_URL")
	if direct == "" {
		t.Skip("KILN_TEST_DIRECT_DATABASE_URL not set; this test runs in the pooled leg only")
	}
	pooled := testPool(t)
	migrator := testPool(t)
	ctx := context.Background()
	directPool, err := pgxpool.New(ctx, direct)
	if err != nil {
		t.Fatalf("direct pool: %v", err)
	}
	t.Cleanup(directPool.Close)

	// Background traffic through the pooler, so consecutive statements from the
	// migrator are actually spread over different server connections.
	hammerCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for hammerCtx.Err() == nil {
				_, _ = pooled.Exec(hammerCtx, `SELECT 1`)
			}
		}()
	}
	t.Cleanup(func() { stop(); wg.Wait() })

	for i := range 30 {
		resetSchema(t, migrator)
		loopCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err := Migrate(loopCtx, migrator)
		cancel()
		if err != nil {
			t.Fatalf("loop %d: Migrate: %v", i, err)
		}
		var held int
		if err := directPool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND granted
			   -- Advisory locks are per database, and other databases on
			   -- the same server run their own migrations.
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			   AND classid = ($1::bigint >> 32)::oid
			   AND objid = ($1::bigint & 4294967295)::oid
			   AND objsubid = 1`, migrateLockKey).Scan(&held); err != nil {
			t.Fatalf("loop %d: read pg_locks: %v", i, err)
		}
		if held != 0 {
			t.Fatalf("loop %d: the migration lock is still held by %d server connection(s) after Migrate returned", i, held)
		}
	}
}

// TestCheckSchemaVersionRefusesGap: the database reports the right maximum but
// one migration beneath it was never recorded.
func TestCheckSchemaVersionRefusesGap(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 3 {
		t.Skip("need at least three migrations to leave a gap below the maximum")
	}
	gap := migrations[len(migrations)/2].version
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, gap); err != nil {
		t.Fatal(err)
	}

	// The blind spot being closed: max(version) still says "up to date".
	if got, err := CurrentVersion(ctx, pool); err != nil || got != SchemaVersion {
		t.Fatalf("CurrentVersion = %d, %v; want %d (the gap is below the maximum)", got, err, SchemaVersion)
	}

	err = CheckSchemaVersion(ctx, pool)
	if err == nil {
		t.Fatal("CheckSchemaVersion accepted a database with an unapplied migration")
	}
	if !strings.Contains(err.Error(), "run `kiln admin migrate`") {
		t.Errorf("error %q does not tell the operator to run `kiln admin migrate`", err)
	}
	if want := fmt.Sprintf("[%d]", gap); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the missing version %s", err, want)
	}
}

func TestCheckSchemaVersionRefusesNewerDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)

	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, 'from_the_future')`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	err := CheckSchemaVersion(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("CheckSchemaVersion = %v, want a newer-than-this-build refusal", err)
	}
}
