package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
)

// bookOfChapters builds a markdown document of n chapters, each carrying a
// marker word that appears nowhere else, so a prompt that holds another
// chapter's text is detectable.
func bookOfChapters(n, bytesPerChapter int) (text string, markers []string) {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		marker := fmt.Sprintf("zzmarker%02d", i)
		markers = append(markers, marker)
		fmt.Fprintf(&b, "# Chapter %02d\n\n", i)
		para := fmt.Sprintf("%s prose about subject %02d.\n", marker, i)
		for b.Len() < i*bytesPerChapter {
			b.WriteString(para)
		}
		b.WriteString("\n")
	}
	return b.String(), markers
}

func TestSplitDocumentSectionsRenderTheirOwnText(t *testing.T) {
	// A 140 KB, 12-chapter document used to become 13 units that each rendered
	// the first 60 KB of the whole file: chapters past that never saw their own
	// words, and every section paid for the same opening pages.
	staging := t.TempDir()
	text, markers := bookOfChapters(12, 12_000)
	if len(text) < 140_000 {
		t.Fatalf("fixture is %d bytes, want a ~140 KB document", len(text))
	}
	write(t, staging, "book.md", text)

	dm := &docmap.Mapper{StageSections: true}
	wm, err := dm.MapDocs(context.Background(), staging, []docmap.Doc{{
		Key: "doc:book.md", Path: "book.md", Title: "The Book", Text: text, Hash: "h",
	}})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}

	var (
		sections int
		total    int
	)
	for _, u := range wm.Units {
		if u.Kind != "doc-section" {
			continue
		}
		sections++
		// A repo root with nothing in it: the unit must resolve through its own
		// recorded root, as it does in a merged map.
		got := renderUnitContext(t.TempDir(), u, buildBudget)
		total += len(got)

		heading, _ := u.Meta["heading"].(string)
		if !strings.Contains(got, "# "+heading) {
			t.Errorf("section %q lost its heading", heading)
		}
		idx := sections - 1
		if !strings.Contains(got, markers[idx]) {
			t.Errorf("section %q did not render its own body (%s)", heading, markers[idx])
		}
		for j, m := range markers {
			if j != idx && strings.Contains(got, m) {
				t.Errorf("section %q rendered sibling text %s", heading, m)
			}
		}
	}
	if sections != 12 {
		t.Fatalf("got %d sections, want 12", sections)
	}
	if limit := len(text) * 12 / 10; total > limit {
		t.Errorf("sections rendered %d bytes in total for a %d byte document, want at most %d",
			total, len(text), limit)
	}
}

func TestStagedSectionsLiveInsideTheStagingDirectory(t *testing.T) {
	// Section files must be removed with the rest of the staging directory;
	// anything written beside it would outlive the run.
	staging := t.TempDir()
	text, _ := bookOfChapters(3, 12_000)

	dm := &docmap.Mapper{StageSections: true}
	wm, err := dm.MapDocs(context.Background(), staging, []docmap.Doc{{
		Key: "doc:a/book.md", Path: "a/book.md", Text: text, Hash: "h",
	}})
	if err != nil {
		t.Fatalf("MapDocs: %v", err)
	}
	for _, u := range wm.Units {
		if u.Kind != "doc-section" {
			continue
		}
		rel := u.Inputs[0]
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			t.Errorf("input %q is not confined to the staging root", rel)
		}
		if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(rel))); err != nil {
			t.Errorf("section input %q was not staged: %v", rel, err)
		}
	}
}
