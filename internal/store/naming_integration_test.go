package store

import (
	"context"
	"testing"
)

// A build renames the documents it read. The filename is a guess made before
// anyone opened the file; once ingest has the text, the name it derives is the
// better one, and the file list should show it.
func TestRenameDocumentsRecordsWhatIngestRead(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []FileRow{
		{WorkspaceID: ws, Path: "Scan_2026-03-14.pdf", DisplayName: "Scan", BlobKey: "b/1", SHA256: "a", Enabled: true},
		{WorkspaceID: ws, Path: "notes.md", DisplayName: "Notes", BlobKey: "b/2", SHA256: "b", Enabled: true},
	} {
		if _, _, err := s.CreateFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	nameOf := func(path string) string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT display_name FROM workspace_files WHERE workspace_id = $1 AND path = $2`,
			ws, path).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	stampOf := func(path string) string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT updated_at::text FROM workspace_files WHERE workspace_id = $1 AND path = $2`,
			ws, path).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := stampOf("notes.md")

	if err := s.RenameDocuments(ctx, ws, map[string]string{
		"Scan_2026-03-14.pdf": "Leadership Insights Syllabus",
		// Unchanged: the build derived the same name it already had.
		"notes.md": "Notes",
		// A file deleted between sync and the write matches nothing rather
		// than failing the run that spent real money.
		"gone.pdf": "Whatever It Was",
		// An empty derivation never overwrites a name with nothing.
		"Scan_2026-03-14.pdf#": "",
	}); err != nil {
		t.Fatal(err)
	}

	if got := nameOf("Scan_2026-03-14.pdf"); got != "Leadership Insights Syllabus" {
		t.Errorf("scan was not renamed: %q", got)
	}
	if got := nameOf("notes.md"); got != "Notes" {
		t.Errorf("unchanged name was disturbed: %q", got)
	}
	// The Ingest list sorts and reports on updated_at, so a rebuild that
	// learned nothing must not make every document look freshly edited.
	if after := stampOf("notes.md"); after != before {
		t.Errorf("a no-op rename touched updated_at: %q -> %q", before, after)
	}
}
