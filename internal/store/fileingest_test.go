package store

import (
	"slices"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// The SQL joins sources on 'doc:upload:' || path, a literal Postgres builds;
// every producer builds the same key through diff.DocKey(diff.UploadOrigin).
// If the two ever drift the join matches nothing and every document reads as
// unread, so the literal is pinned here against the Go side for the path
// shapes an upload can take.
func TestUploadDocKeyParity(t *testing.T) {
	paths := []string{
		"plan.md",
		"notes/plan.md",
		"7 Questions to Assess Your Business Model Design.pdf",
		"q3#final.pdf",
		"Übersicht – Straße.pdf",
		"日本語 レポート.docx",
		"a%b_c.pdf",
		"README.md",
	}
	for _, p := range paths {
		want := string(diff.DocKey(diff.UploadOrigin(p)))
		if got := uploadDocKeyPrefix + p; got != want {
			t.Errorf("%q: SQL literal gives %q, diff gives %q", p, got, want)
		}
	}

	// The constant is only a promise if the SQL actually spells it.
	literal := "'" + uploadDocKeyPrefix + "' || f.path"
	if !strings.Contains(listFilesWithIngestSQL, literal) {
		t.Errorf("listFilesWithIngestSQL does not join on %s", literal)
	}
}

// sortFileSkips fixes the lock order and collapses a path given twice, so the
// update never visits one target row from two source rows.
func TestSortFileSkips(t *testing.T) {
	in := []FileSkip{
		{Path: "b.pdf", SHA256: "b1", Class: "no_text"},
		{Path: "a.pdf", SHA256: "a1", Class: "extractor_error"},
		{Path: "c.pdf", SHA256: "c1", Class: "no_text"},
		{Path: "a.pdf", SHA256: "a2", Class: "no_text"},
	}
	orig := slices.Clone(in)

	got := sortFileSkips(in)
	want := []FileSkip{
		{Path: "a.pdf", SHA256: "a2", Class: "no_text"},
		{Path: "b.pdf", SHA256: "b1", Class: "no_text"},
		{Path: "c.pdf", SHA256: "c1", Class: "no_text"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("sortFileSkips = %+v, want %+v", got, want)
	}
	if !slices.Equal(in, orig) {
		t.Errorf("sortFileSkips mutated its input: %+v", in)
	}
	if got := sortFileSkips(nil); len(got) != 0 {
		t.Errorf("sortFileSkips(nil) = %+v, want empty", got)
	}
}
