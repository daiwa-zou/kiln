package store

import (
	"context"
	"slices"
	"testing"
)

// TestMigrateIngestStatusOnExistingRows applies 021 to a database shaped like
// the dev one at 020: documents with and without sources, a section source, a
// paused file. The rows must come through untouched with the new columns NULL,
// and a 020 binary's view of the applied set must still be whole below it.
func TestMigrateIngestStatusOnExistingRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	resetSchema(t, pool)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	const ingestVersion = 21
	idx := slices.IndexFunc(migrations, func(m migration) bool { return m.version == ingestVersion })
	if idx < 0 {
		t.Fatalf("migration %d is not embedded", ingestVersion)
	}
	if _, err := migrateWith(ctx, pool, migrations[:idx]); err != nil {
		t.Fatalf("migrate to %d: %v", migrations[idx-1].version, err)
	}

	// Rows as the previous binary wrote them: raw SQL, because this binary's
	// CreateFile already names the new columns.
	s := NewWikiStore(pool)
	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace_files (workspace_id, path, display_name, blob_key, size_bytes, sha256, enabled)
	      VALUES ($1, 'read.pdf', 'Read', 'b1', 10, 'r1', TRUE),
	             ($1, 'waiting.pdf', 'Waiting', 'b2', 10, 'w1', TRUE),
	             ($1, 'paused.pdf', 'Paused', 'b3', 10, 'p1', FALSE)`, ws)
	exec(`INSERT INTO sources (workspace_id, key, kind, input_hash, files_written)
	      VALUES ($1, 'doc:upload:read.pdf', 'doc', 'h1', '["sources/read.md"]'),
	             ($1, 'doc:upload:read.pdf#intro', 'doc', 'h2', '["sources/read-intro.md"]')`, ws)
	exec(`INSERT INTO runs (workspace_id, trigger, status, started_at) VALUES ($1, 'manual', 'succeeded', now())`, ws)

	snapshot := func() string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, `
			SELECT json_agg(f ORDER BY f.path)::text FROM (
				SELECT id, workspace_id, path, display_name, blob_key, size_bytes, content_type,
				       sha256, uploaded_by, enabled, created_at, updated_at
				FROM workspace_files) f`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()

	applied, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if want := []int{ingestVersion}; !slices.Equal(applied, want) {
		t.Errorf("applied %v, want %v", applied, want)
	}
	if got, err := CurrentVersion(ctx, pool); err != nil || got != ingestVersion || got != SchemaVersion {
		t.Errorf("CurrentVersion = %d, %v; want %d (= SchemaVersion %d)", got, err, ingestVersion, SchemaVersion)
	}
	if err := CheckSchemaVersion(ctx, pool); err != nil {
		t.Errorf("CheckSchemaVersion: %v", err)
	}
	if after := snapshot(); after != before {
		t.Errorf("existing rows changed:\nbefore %s\nafter  %s", before, after)
	}
	var rows, touched int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE skip_class IS NOT NULL OR skip_sha256 IS NOT NULL
		                           OR staged_at IS NOT NULL OR content_changed_at IS NOT NULL)
		FROM workspace_files`).Scan(&rows, &touched); err != nil {
		t.Fatalf("new columns: %v", err)
	}
	if rows != 3 || touched != 0 {
		t.Errorf("%d rows, %d with a non-NULL new column; want 3 and 0 (no backfill)", rows, touched)
	}

	// The new reader works on the old rows: nothing skipped, content as of
	// creation, the section source counted into its document.
	list, bench, err := s.ListFilesWithIngest(ctx, ws)
	if err != nil {
		t.Fatalf("ListFilesWithIngest on migrated rows: %v", err)
	}
	if len(list) != 3 || bench.PendingRun || bench.BudgetPaused {
		t.Fatalf("list = %+v, bench = %+v", list, bench)
	}
	for _, f := range list {
		if f.SkipClass != nil || f.SkipLive || f.StagedAt != nil || !f.ContentAt.Equal(f.CreatedAt) {
			t.Errorf("%s: skip=%v live=%v staged=%v content=%v created=%v", f.Path, f.SkipClass, f.SkipLive, f.StagedAt, f.ContentAt, f.CreatedAt)
		}
		if f.Path == "read.pdf" && (!f.HasSource || f.Pages == nil || *f.Pages != 2 || f.ReadAt == nil) {
			t.Errorf("read.pdf: %+v", f)
		}
		if f.Path == "paused.pdf" && f.Enabled {
			t.Error("paused.pdf resumed by the migration")
		}
	}

	// The runner records the version, so a second Migrate applies nothing.
	applied, err = Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second Migrate applied %v, want nothing", applied)
	}

	// Rolling back: DELETE the version row and a 020 binary accepts the
	// database again, columns and all.
	versions, err := AppliedVersions(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSchema(versions[:idx], migrations[:idx]); err != nil {
		t.Errorf("a %d binary would refuse the applied set below %d: %v", migrations[idx-1].version, ingestVersion, err)
	}
	if err := checkSchema(versions, migrations[:idx]); err == nil {
		t.Error("a 020 binary accepted a v21 database; the rollback note would be wrong")
	}
}
