package blob

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/config"
)

func TestFileKeyShape(t *testing.T) {
	key := FileKey("ws-uuid", "file-uuid")
	if key != "ws/ws-uuid/uploads/file-uuid" {
		t.Fatalf("unexpected key %q", key)
	}
	if err := validKey(key); err != nil {
		t.Fatalf("canonical key rejected: %v", err)
	}
}

func TestValidKey(t *testing.T) {
	bad := []string{
		"",
		"/abs",
		"a//b",
		"../escape",
		"a/../b",
		"a/./b",
		".",
		"a/",
	}
	for _, key := range bad {
		if err := validKey(key); err == nil {
			t.Errorf("validKey(%q) accepted", key)
		}
	}
	good := []string{"a", "a/b", "ws/id/uploads/id2", "with-dash_and.dot"}
	for _, key := range good {
		if err := validKey(key); err != nil {
			t.Errorf("validKey(%q) rejected: %v", key, err)
		}
	}
}

func TestOpenSelectsDriver(t *testing.T) {
	fs, err := Open(config.Storage{Backend: config.BackendFS, Path: t.TempDir()})
	if err != nil {
		t.Fatalf("open fs: %v", err)
	}
	if _, ok := fs.(*fsStore); !ok {
		t.Fatalf("want *fsStore, got %T", fs)
	}
	if _, err := Open(config.Storage{Backend: "carrier-pigeon"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
	if _, err := Open(config.Storage{Backend: config.BackendFS}); err == nil {
		t.Fatal("fs backend without path accepted")
	}
}

func TestFSRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := openFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := FileKey("ws1", "f1")
	content := []byte("fired clay")

	if err := s.Put(ctx, key, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("got %q err %v, want %q", got, err, content)
	}

	// Replace under the same key.
	if err := s.Put(ctx, key, strings.NewReader("second firing"), -1); err != nil {
		t.Fatalf("put replace: %v", err)
	}
	rc, err = s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "second firing" {
		t.Fatalf("replace read back %q", got)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, key); err == nil {
		t.Fatal("get after delete succeeded")
	}
	// Idempotent delete.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestFSPutAtomicity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := openFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A reader that fails midway must leave no blob and no visible partial.
	failing := io.MultiReader(strings.NewReader("partial"), errReader{})
	if err := s.Put(ctx, "ws/w/uploads/f", failing, -1); err == nil {
		t.Fatal("put with failing reader succeeded")
	}
	if _, err := s.Get(ctx, "ws/w/uploads/f"); err == nil {
		t.Fatal("partial blob visible after failed put")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "ws", "w", "uploads"))
	if err == nil {
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("unexpected non-temp file %q after failed put", e.Name())
			}
		}
	}
}

func TestFSRejectsEscape(t *testing.T) {
	ctx := context.Background()
	s, err := openFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "../outside", strings.NewReader("x"), -1); err == nil {
		t.Fatal("traversal key accepted")
	}
	if _, err := s.Get(ctx, "../outside"); err == nil {
		t.Fatal("traversal get accepted")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
