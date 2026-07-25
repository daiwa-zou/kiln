package repomap

import (
	"context"
	"os"
	"testing"
)

// TestScanRealRepo runs against a real repository on disk, addressed by
// KILN_SCAN_REPO, and prints the rendered map. It is a manual inspection aid
// rather than an assertion, so it is skipped unless that variable is set:
//
//	KILN_SCAN_REPO=~/Developer/Repositories/watchtower \
//	  go test ./internal/mapper/repomap -run TestScanRealRepo -v
func TestScanRealRepo(t *testing.T) {
	root := os.Getenv("KILN_SCAN_REPO")
	if root == "" {
		t.Skip("KILN_SCAN_REPO not set; skipping real-repository scan")
	}

	s := &Scanner{}
	rm, err := s.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan(%s): %v", root, err)
	}

	t.Logf("\n%s", rm.Render())

	wm := rm.ToWorkspaceMap()
	t.Logf("units=%d edges=%d hash=%s", len(wm.Units), len(wm.Edges), rm.Hash[:12])

	// Sanity properties that must hold for any real repository.
	if len(rm.Modules) == 0 {
		t.Error("no modules detected")
	}
	if rm.Hash == "" {
		t.Error("map hash is empty")
	}
	for _, u := range wm.Units {
		if u.Key == "" || u.Slug == "" {
			t.Errorf("unit with empty key or slug: %+v", u)
		}
	}
}
