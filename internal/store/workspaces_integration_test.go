package store

import (
	"context"
	"errors"
	"testing"
)

// Deleting a bench has to take everything scoped to it with it, and hand back
// the blob keys the database cannot reach, or an operator who deletes a bench
// is left with rows in tables nobody lists and bytes in a bucket nobody bills
// them for on purpose.
func TestDeleteWorkspaceCascadesAndReportsBlobs(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "doomed", "doomed")
	if err != nil {
		t.Fatal(err)
	}
	keep, err := s.EnsureWorkspace(ctx, "local", "survivor", "survivor")
	if err != nil {
		t.Fatal(err)
	}

	// One row in a cascading table, and two uploads whose bytes live outside
	// the database.
	if _, _, err := s.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "a.pdf", BlobKey: "blobs/a", SizeBytes: 1,
		ContentType: "application/pdf", SHA256: "aa", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "b.pdf", BlobKey: "blobs/b", SizeBytes: 1,
		ContentType: "application/pdf", SHA256: "bb", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateFile(ctx, FileRow{
		WorkspaceID: keep, Path: "c.pdf", BlobKey: "blobs/c", SizeBytes: 1,
		ContentType: "application/pdf", SHA256: "cc", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueRun(ctx, ws, "manual", ""); err != nil {
		t.Fatal(err)
	}

	keys, err := s.DeleteWorkspace(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want both blob keys back, got %v", keys)
	}
	// Only this bench's blobs: reporting a neighbour's key would have the
	// caller delete bytes another bench still points at.
	for _, k := range keys {
		if k == "blobs/c" {
			t.Fatalf("returned another workspace's blob key: %v", keys)
		}
	}

	var files, runs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM workspace_files WHERE workspace_id = $1`, ws).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE workspace_id = $1`, ws).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if files != 0 || runs != 0 {
		t.Fatalf("cascade left rows behind: files=%d runs=%d", files, runs)
	}

	// The neighbour is untouched, and so is the org both shared: dissolving it
	// would revoke access to a tenant that still has benches in it.
	var survivors, orgs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM workspace_files WHERE workspace_id = $1`, keep).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orgs WHERE slug = 'local'`).Scan(&orgs); err != nil {
		t.Fatal(err)
	}
	if survivors != 1 || orgs != 1 {
		t.Fatalf("blast radius too wide: survivor files=%d orgs=%d", survivors, orgs)
	}

	// Deleting it again is a 404, not a silent success: the API leans on this
	// to tell "gone" from "never existed".
	if _, err := s.DeleteWorkspace(ctx, ws); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}
