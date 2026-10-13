package upload

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

	set, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
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
	if _, ok := keys["doc:upload:notes.md"]; !ok {
		t.Errorf("notes.md missing: %v", itemKeys(set))
	}
	if _, ok := keys["doc:upload:reports/q3.txt"]; !ok {
		t.Errorf("nested document missing: %v", itemKeys(set))
	}

	// Unsupported types, dotfiles, and vendored directories are not material.
	for _, unwanted := range []string{"doc:upload:ignored.zip", "doc:upload:.hidden.md", "doc:upload:node_modules/x.md"} {
		if _, ok := keys[unwanted]; ok {
			t.Errorf("%s should not have been ingested", unwanted)
		}
	}

	// A heading beats the filename as a title.
	if got := keys["doc:upload:notes.md"].Title; got != "Meeting Notes" {
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

// A file the walk leaves out by name is reported, not dropped. On a server
// the upload API accepts any supported extension under any relative path, so
// a ._report.pdf or a vault/.obsidian/x.md is staged like the rest; without a
// skip it would read as never ingested with nothing to say why. A hidden file
// of a format nothing reads is not a candidate anywhere and stays silent.
func TestSyncReportsHiddenPathsAsSkips(t *testing.T) {
	files := map[string]string{
		"good.md":                   "# Fine\n\nReadable.\n",
		"._x.md":                    "# AppleDouble shadow\n",
		"vault/.obsidian/notes.md":  "# Vault config\n",
		"node_modules/a/readme.md":  "# A package\n",
		".DS_Store":                 "binary",
		"vault/.git/config":         "[core]",
		"vault/__MACOSX/._stale.md": "# Stale\n",
	}
	hiddenSkips := func(t *testing.T, set *connector.SourceSet) []string {
		t.Helper()
		if len(set.Items) != 1 || set.Items[0].Key != "doc:upload:good.md" {
			t.Errorf("items = %v, want only good.md", itemKeys(set))
		}
		var got []string
		for _, sk := range PayloadOf(set).Skipped {
			if sk.Class != SkipHiddenPath || sk.Partial {
				t.Errorf("%s: skip = %+v, want class %s", sk.Path, sk, SkipHiddenPath)
			}
			got = append(got, sk.Path)
		}
		return got
	}

	// A local folder: the excluded folders are pruned unreported, as they
	// always were (a vendored tree is nobody's upload); a dot-prefixed file is
	// reported.
	set, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": fixture(t, files)}, t.TempDir())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got, want := hiddenSkips(t, set), []string{"._x.md"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("local folder hidden skips = %v, want %v", got, want)
	}

	// The bench's materialized uploads: every row is the member's, so the
	// ones left out by name are reported, readable formats only.
	set, err = (&Connector{}).Sync(context.Background(), connector.Config{"path": fixture(t, files), "materialized": true}, t.TempDir())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	want := []string{"._x.md", "node_modules/a/readme.md", "vault/.obsidian/notes.md", "vault/__MACOSX/._stale.md"}
	if got := hiddenSkips(t, set); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("materialized hidden skips = %v, want %v (sorted, readable formats only)", got, want)
	}
}

// Only a dot-prefixed file name hides a document, never a dot-prefixed folder:
// benches keep documents in .notes/ or docs/.wip/, and hiding those would file
// a deletion review for every page they produced.
func TestSyncReadsDocumentsInDotFolders(t *testing.T) {
	files := map[string]string{
		"good.md":           "# Fine\n\nReadable.\n",
		".notes/plan.md":    "# Plan\n\nKept in a dot-folder.\n",
		"docs/.wip/idea.md": "# Idea\n\nNested under one.\n",
	}
	for _, materialized := range []bool{false, true} {
		cfg := connector.Config{"path": fixture(t, files), "materialized": materialized}
		set, err := (&Connector{}).Sync(context.Background(), cfg, t.TempDir())
		if err != nil {
			t.Fatalf("Sync(materialized=%v): %v", materialized, err)
		}
		want := []string{"doc:upload:.notes/plan.md", "doc:upload:docs/.wip/idea.md", "doc:upload:good.md"}
		if got := itemKeys(set); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("materialized=%v: items = %v, want %v; skips = %+v", materialized, got, want, PayloadOf(set).Skipped)
		}
	}
}

// Every Skip* constant the package declares is in SkipClasses, so a class added
// to the constants alone cannot fold into extractor_error on the API and the
// metric without this failing. Read from the source, since Go offers no way to
// enumerate a package's constants at run time.
func TestSkipClassesListsEveryDeclaredConstant(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var declared []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, name := range vs.Names {
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !strings.HasPrefix(name.Name, "Skip") || !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", name.Name, err)
					}
					declared = append(declared, value)
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no Skip* string constants found; the parse is looking in the wrong place")
	}
	for _, class := range declared {
		if !slices.Contains(SkipClasses, class) {
			t.Errorf("class %q is declared but not in SkipClasses", class)
		}
	}
	if len(declared) != len(SkipClasses) {
		t.Errorf("SkipClasses has %d entries, the package declares %d", len(SkipClasses), len(declared))
	}
}

// SkipClasses is what the API's sentence table and the metric's label list
// check themselves against, so it must name every class a Skip can carry.
func TestSkipClassesNamesEveryClass(t *testing.T) {
	want := []string{SkipNoText, SkipExtractorMissing, SkipUnsupportedType, SkipTimedOut, SkipExtractorError, SkipHiddenPath}
	if strings.Join(SkipClasses, ",") != strings.Join(want, ",") {
		t.Errorf("SkipClasses = %v, want %v", SkipClasses, want)
	}
	for _, err := range []error{extract.ErrToolMissing, extract.ErrNoExtractor, extract.ErrTimedOut, errors.New("other")} {
		if class := skipClass(err); !slices.Contains(SkipClasses, class) {
			t.Errorf("skipClass(%v) = %q, which SkipClasses does not list", err, class)
		}
	}
}

func TestSyncSkipsEmptyExtractions(t *testing.T) {
	root := fixture(t, map[string]string{
		"empty.md": "   \n\n  \n",
		"real.md":  "# Real\n\nContent.\n",
	})

	set, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// A page about nothing still costs a model call.
	if len(set.Items) != 1 || set.Items[0].Key != "doc:upload:real.md" {
		t.Errorf("items = %v, want only the document with content", itemKeys(set))
	}
}

func TestSyncStagesExtractedText(t *testing.T) {
	root := fixture(t, map[string]string{"notes.md": "# Notes\n\nBody.\n"})
	staging := t.TempDir()

	set, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": root}, staging)
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
	first, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": fixture(t, files)}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": fixture(t, files)}, t.TempDir())
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

	first, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := (&Connector{}).Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
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
	c := &Connector{}
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

func TestSkipClassIsDecidedByTheExtractSentinels(t *testing.T) {
	// The class is what a server build persists, so it comes from errors.Is on
	// the sentinels, not from the prose skipReason prints. The timeout row is
	// the real error a hanging converter produces, not a hand-written copy of
	// its message: the message is not the contract, the sentinel is.
	hanging := filepath.Join(t.TempDir(), "hanging-pdftotext")
	if err := os.WriteFile(hanging, []byte("#!/bin/sh\nexec sleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	es := extract.Extractors{&extract.PDFExtractor{Binary: hanging, Timeout: 100 * time.Millisecond}}
	_, timedOut := es.Extract(context.Background(), filepath.Join(fixture(t, map[string]string{"a.pdf": "%PDF"}), "a.pdf"))
	if timedOut == nil {
		t.Fatal("a hanging converter did not fail")
	}

	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("extract: %w: pdf needs a tool that is not installed", extract.ErrToolMissing), SkipExtractorMissing},
		{fmt.Errorf("%w: office", extract.ErrNoExtractor), SkipUnsupportedType},
		{fmt.Errorf("extract: a.pdf: %w", context.DeadlineExceeded), SkipTimedOut},
		{timedOut, SkipTimedOut},
		{errors.New("extract: a.pdf (pdftotext): pdftotext timed out after 2m0s"), SkipExtractorError},
		{errors.New("extract: a.pdf (pdftotext): pdftotext: exit status 1 (Syntax Error)"), SkipExtractorError},
	} {
		if got := skipClass(tc.err); got != tc.want {
			t.Errorf("skipClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestSyncFlagsAFigureFailureAsPartial(t *testing.T) {
	// echo stands in for pdftotext: it prints its arguments, which is text
	// enough for a page. The figure tool is missing, so the figures fail and
	// the document is reported partial: a page was written, pictures were lost.
	root := fixture(t, map[string]string{"report.pdf": "%PDF-1.4 not really\n"})
	c := &Connector{Extractors: extract.Extractors{
		&extract.PDFExtractor{Binary: "/bin/echo", FigureBinary: "kiln-nonexistent-pdfimages"},
	}}
	set, err := c.Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Items) != 1 {
		t.Fatalf("items = %v, want the report's page", itemKeys(set))
	}
	skips := PayloadOf(set).Skipped
	if len(skips) != 1 || !skips[0].Partial || skips[0].Class != SkipExtractorError ||
		!strings.HasPrefix(skips[0].Reason, "figures could not be read: ") {
		t.Errorf("skips = %+v, want one partial extractor_error with the figures reason", skips)
	}
}

func TestSyncClassifiesAndFlagsPartialSkips(t *testing.T) {
	root := fixture(t, map[string]string{
		"empty.md": "   \n",
		"bad.pdf":  "%PDF-1.4 not really\n",
	})
	c := &Connector{Extractors: extract.Extractors{
		&extract.PassthroughExtractor{},
		&extract.PDFExtractor{Binary: "kiln-nonexistent-pdftotext"},
	}}
	set, err := c.Sync(context.Background(), connector.Config{"path": root}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SkipReason{}
	for _, s := range PayloadOf(set).Skipped {
		got[s.Path] = s
	}
	// The printed reasons are unchanged; the class rides beside them.
	if s := got["empty.md"]; s.Reason != "extracted no text" || s.Class != SkipNoText || s.Partial {
		t.Errorf("empty.md skip = %+v", s)
	}
	if s := got["bad.pdf"]; s.Reason != "required extraction tool is not installed" || s.Class != SkipExtractorMissing || s.Partial {
		t.Errorf("bad.pdf skip = %+v", s)
	}
}
