package upload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/connector"
	"github.com/daiwa-zou/kiln/internal/extract"
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

func TestSyncExtractsDocuments(t *testing.T) {
	root := fixture(t, map[string]string{
		"notes.md":          "# Meeting Notes\n\nWe decided X.\n",
		"reports/q3.txt":    "Quarterly figures.\n",
		"ignored.zip":       "PK\x03\x04",
		".hidden.md":        "# Hidden\n",
		"node_modules/x.md": "# Vendored\n",
	})

	set, err := New().Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	keys := map[string]connector.Item{}
	for _, item := range set.Items {
		keys[item.Key] = item
	}
	if len(keys) != 2 {
		t.Fatalf("got %d items, want 2: %v", len(keys), itemKeys(set))
	}
	if _, ok := keys["doc:notes.md"]; !ok {
		t.Errorf("notes.md missing: %v", itemKeys(set))
	}
	if _, ok := keys["doc:reports/q3.txt"]; !ok {
		t.Errorf("nested document missing: %v", itemKeys(set))
	}

	// Unsupported types, dotfiles, and vendored directories are not material.
	for _, unwanted := range []string{"doc:ignored.zip", "doc:.hidden.md", "doc:node_modules/x.md"} {
		if _, ok := keys[unwanted]; ok {
			t.Errorf("%s should not have been ingested", unwanted)
		}
	}

	// A heading beats the filename as a title.
	if got := keys["doc:notes.md"].Title; got != "Meeting Notes" {
		t.Errorf("Title = %q, want the first heading", got)
	}
}

func TestSyncSkipsUnextractableFilesWithoutFailing(t *testing.T) {
	// One file needing a tool that is not installed must not cost the others.
	// A folder of fifty documents should not be lost to one bad PDF.
	root := fixture(t, map[string]string{
		"good.md":  "# Fine\n\nReadable.\n",
		"bad.pdf":  "%PDF-1.4 not really\n",
		"good2.md": "# Also fine\n",
	})

	c := &Connector{Extractors: extract.Extractors{
		&extract.PassthroughExtractor{},
		&extract.PDFExtractor{Binary: "kiln-nonexistent-pdftotext"},
	}}

	set, err := c.Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatalf("Sync failed instead of skipping: %v", err)
	}
	if len(set.Items) != 2 {
		t.Errorf("got %d items, want the 2 readable ones: %v", len(set.Items), itemKeys(set))
	}

	payload := PayloadOf(set)
	if payload == nil || len(payload.Skipped) != 1 {
		t.Fatalf("skips = %+v, want one reported", payload)
	}
	// The skip must say a tool is missing, not that the document is broken.
	if !strings.Contains(payload.Skipped[0].Reason, "not installed") {
		t.Errorf("skip reason = %q, want it to name the missing tool", payload.Skipped[0].Reason)
	}
}

func TestSyncSkipsEmptyExtractions(t *testing.T) {
	root := fixture(t, map[string]string{
		"empty.md": "   \n\n  \n",
		"real.md":  "# Real\n\nContent.\n",
	})

	set, err := New().Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// A page about nothing still costs a model call.
	if len(set.Items) != 1 || set.Items[0].Key != "doc:real.md" {
		t.Errorf("items = %v, want only the document with content", itemKeys(set))
	}
}

func TestSyncStagesExtractedText(t *testing.T) {
	root := fixture(t, map[string]string{"notes.md": "# Notes\n\nBody.\n"})
	staging := t.TempDir()

	set, err := New().Sync(context.Background(), connector.Config{"path": root}, staging)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Staging means a mapper and the agent read text without re-running the
	// converter for every unit.
	staged := filepath.Join(staging, set.Items[0].Path)
	body, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("staged file missing: %v", err)
	}
	if !strings.Contains(string(body), "# Notes") {
		t.Errorf("staged content = %q", body)
	}
}

func TestSyncHashesAreContentOnly(t *testing.T) {
	files := map[string]string{"notes.md": "# Notes\n\nSame body.\n"}

	// The same document in two folders must hash identically, or a restaged
	// upload would look changed and regenerate for nothing.
	first, err := New().Sync(context.Background(), connector.Config{"path": fixture(t, files)}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Sync(context.Background(), connector.Config{"path": fixture(t, files)}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if first.Items[0].Hash != second.Items[0].Hash {
		t.Error("hash depends on where the document was staged")
	}
}

func TestSyncIsDeterministic(t *testing.T) {
	root := fixture(t, map[string]string{
		"z.md": "# Z\n", "a.md": "# A\n", "m/nested.md": "# M\n",
	})

	first, err := New().Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New().Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Walk order must be sorted, or item order would vary between syncs.
	for i := range first.Items {
		if first.Items[i].Key != second.Items[i].Key {
			t.Fatalf("item %d differs between syncs: %q vs %q",
				i, first.Items[i].Key, second.Items[i].Key)
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
}

func TestRegisteredAlongsideGit(t *testing.T) {
	// Two registered connectors is the point: an interface with one
	// implementation is an assumption, not a seam.
	got, err := connector.Get("upload")
	if err != nil {
		t.Fatalf("upload connector not registered: %v", err)
	}
	if got.Kind() != "upload" {
		t.Errorf("Kind = %q", got.Kind())
	}
}

func TestPayloadOfForeignSetIsNil(t *testing.T) {
	// A caller written against uploads must degrade on a git set, not panic.
	if got := PayloadOf(&connector.SourceSet{Kind: "git", Native: "something else"}); got != nil {
		t.Errorf("PayloadOf = %+v, want nil for another connector's set", got)
	}
	if got := PayloadOf(nil); got != nil {
		t.Error("PayloadOf(nil) should be nil")
	}
}

func itemKeys(set *connector.SourceSet) []string {
	out := make([]string, 0, len(set.Items))
	for _, item := range set.Items {
		out = append(out, item.Key)
	}
	return out
}
