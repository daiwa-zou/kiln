package store

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

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
