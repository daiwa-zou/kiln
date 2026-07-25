package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daiwa-zou/kiln/internal/connector"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()

	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSyncDescribesModulesAndDocs(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod":    "module example.com/x\n\ngo 1.24\n",
		"main.go":   "package main\n\nfunc main() {}\n",
		"README.md": "# My Project\n\nWhat it does.\n",
	})

	set, err := New().Sync(context.Background(), connector.Config{"path": root}, "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if set.Kind != "git" {
		t.Errorf("Kind = %q", set.Kind)
	}
	if set.Root != root {
		t.Errorf("Root = %q, want %q", set.Root, root)
	}

	byKey := map[string]connector.Item{}
	for _, item := range set.Items {
		byKey[item.Key] = item
	}
	if _, ok := byKey["module:root"]; !ok {
		t.Errorf("no root module item: %v", keys(byKey))
	}
	if _, ok := byKey["doc:README.md"]; !ok {
		t.Errorf("no README doc item: %v", keys(byKey))
	}

	// Every item must carry a hash: it is the gate that makes an unchanged run
	// free, so a blank one would silently force regeneration forever.
	for _, item := range set.Items {
		if item.Hash == "" {
			t.Errorf("item %s has no hash", item.Key)
		}
	}
}

func TestSyncSkipsEmptyModules(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	// An empty directory, as moneypal/butler is on disk.
	if err := os.MkdirAll(filepath.Join(root, "butler"), 0o755); err != nil {
		t.Fatal(err)
	}

	set, err := New().Sync(context.Background(), connector.Config{"path": root}, "")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	for _, item := range set.Items {
		if item.Path == "butler" {
			t.Error("an empty directory became an item; sending it to a model spends money on nothing")
		}
	}
}

func TestSyncIsStableAcrossRuns(t *testing.T) {
	files := map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}

	// Two different directories with identical content must produce identical
	// hashes, or a per-run checkout path would make every build look changed.
	first, err := New().Sync(context.Background(), connector.Config{"path": fixture(t, files)}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Sync(context.Background(), connector.Config{"path": fixture(t, files)}, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(first.Items) != len(second.Items) {
		t.Fatalf("item counts differ: %d vs %d", len(first.Items), len(second.Items))
	}
	for i := range first.Items {
		if first.Items[i].Key != second.Items[i].Key {
			t.Errorf("key %d differs: %q vs %q", i, first.Items[i].Key, second.Items[i].Key)
		}
		if first.Items[i].Hash != second.Items[i].Hash {
			t.Errorf("hash for %s depends on the checkout path", first.Items[i].Key)
		}
	}
}

func TestSyncRejectsBadPaths(t *testing.T) {
	c := New()
	ctx := context.Background()

	if _, err := c.Sync(ctx, connector.Config{}, ""); err == nil {
		t.Error("Sync accepted a config with no path")
	}
	if _, err := c.Sync(ctx, connector.Config{"path": "/nonexistent/nowhere"}, ""); err == nil {
		t.Error("Sync accepted a nonexistent path")
	}

	file := filepath.Join(fixture(t, map[string]string{"x.txt": "hi"}), "x.txt")
	if _, err := c.Sync(ctx, connector.Config{"path": file}, ""); err == nil {
		t.Error("Sync accepted a file where a directory is required")
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	// The blank-import registration is what lets the build command look a
	// connector up by kind rather than constructing it directly.
	got, err := connector.Get("git")
	if err != nil {
		t.Fatalf("git connector not registered: %v", err)
	}
	if got.Trigger() != connector.TriggerManual {
		t.Errorf("Trigger = %q, want manual for a local path", got.Trigger())
	}
}

func keys(m map[string]connector.Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
