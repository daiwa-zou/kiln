package store

import (
	"context"
	"errors"
	"testing"
)

func TestFilesCRUD(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	freshSchema(t, pool)
	s := NewWikiStore(pool)

	ws, err := s.EnsureWorkspace(ctx, "local", "bench", "bench")
	if err != nil {
		t.Fatal(err)
	}

	id, replaced, err := s.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "notes/plan.md", BlobKey: "ws/" + ws + "/uploads/f1",
		SizeBytes: 12, ContentType: "text/markdown", SHA256: "aaaa",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == "" || replaced != "" {
		t.Fatalf("create returned id=%q replaced=%q", id, replaced)
	}

	files, err := s.ListFiles(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "notes/plan.md" || files[0].SHA256 != "aaaa" {
		t.Fatalf("list after create: %+v", files)
	}

	// Re-uploading the same path replaces the row and reports the old blob.
	id2, replaced, err := s.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "notes/plan.md", BlobKey: "ws/" + ws + "/uploads/f2",
		SizeBytes: 20, SHA256: "bbbb",
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced != "ws/"+ws+"/uploads/f1" {
		t.Fatalf("replace returned old blob %q", replaced)
	}
	if id2 != id {
		t.Fatalf("replace changed row id %q -> %q", id, id2)
	}
	files, _ = s.ListFiles(ctx, ws)
	if len(files) != 1 || files[0].BlobKey != "ws/"+ws+"/uploads/f2" || files[0].SHA256 != "bbbb" {
		t.Fatalf("list after replace: %+v", files)
	}

	// Pause round-trips, and re-uploading a paused path resumes it.
	if err := s.SetFileEnabled(ctx, ws, id, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	files, _ = s.ListFiles(ctx, ws)
	if len(files) != 1 || files[0].Enabled {
		t.Fatalf("list after pause: %+v", files)
	}
	if _, _, err := s.CreateFile(ctx, FileRow{
		WorkspaceID: ws, Path: "notes/plan.md", BlobKey: "ws/" + ws + "/uploads/f3",
		SizeBytes: 5, SHA256: "cccc",
	}); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	files, _ = s.ListFiles(ctx, ws)
	if len(files) != 1 || !files[0].Enabled {
		t.Fatalf("re-upload did not resume: %+v", files)
	}

	// Pause and delete are workspace-scoped: another tenant sees nothing.
	other, err := s.EnsureWorkspace(ctx, "local", "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFileEnabled(ctx, other, id, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant pause: %v, want ErrNotFound", err)
	}
	if _, err := s.DeleteFile(ctx, other, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete: %v, want ErrNotFound", err)
	}
	if list, _ := s.ListFiles(ctx, other); len(list) != 0 {
		t.Fatalf("cross-tenant list leaked %+v", list)
	}

	blobKey, err := s.DeleteFile(ctx, ws, id)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if blobKey != "ws/"+ws+"/uploads/f3" {
		t.Fatalf("delete returned blob %q, want the re-uploaded f3", blobKey)
	}
	if _, err := s.DeleteFile(ctx, ws, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
	if files, _ := s.ListFiles(ctx, ws); len(files) != 0 {
		t.Fatalf("list after delete: %+v", files)
	}
}
