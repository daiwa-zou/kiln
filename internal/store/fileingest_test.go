package store

import (
	"slices"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
)

// The SQL joins sources on uploadDocKeyPrefix || path, so it assumes the key
// of an uploaded document is its path appended to a fixed prefix. diff owns
// the key; if it ever did more than append (escaped a character, say) the
// join would match nothing and every document would read as unread, so the
// assumption is pinned for the path shapes an upload can take.
func TestUploadDocKeyIsPrefixPlusPath(t *testing.T) {
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
			t.Errorf("%q: prefix || path gives %q, diff gives %q", p, got, want)
		}
	}
}

// sortFileSkips collapses a path given twice and orders the batch, so the
// update never visits one target row from two source rows and the changed
// list is deterministic.
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
