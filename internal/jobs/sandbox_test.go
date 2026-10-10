package jobs

import (
	"reflect"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

// flagValues is every value a variadic flag takes: the arguments after it up to
// the next flag.
func flagValues(args []string, flag string) []string {
	for i, a := range args {
		if a != flag {
			continue
		}
		var vals []string
		for _, v := range args[i+1:] {
			if strings.HasPrefix(v, "--") {
				break
			}
			vals = append(vals, v)
		}
		return vals
	}
	return nil
}

// The scratch directory is now the CLI's working directory, so tool state can
// land in it. Pages are still found, and state in a dot directory is not one.
func TestCollectPagesIgnoresToolStateInTheWorkingDirectory(t *testing.T) {
	scratch := t.TempDir()
	writeTree(t, scratch, map[string]string{
		"entities/ripple.md":    validPage("entity", "Ripple"),
		".claude/notes.md":      "# not a page\n",
		".claude/sub/memory.md": "not a page either",
	})

	pages, violations, err := collectPages(scratch)
	if err != nil {
		t.Fatalf("collectPages: %v", err)
	}
	if len(violations) != 0 || len(pages) != 1 || pages[0].Path != "entities/ripple.md" {
		t.Errorf("pages = %v, violations = %v, want only entities/ripple.md", pages, violations)
	}
}

// Under the CLI's --restricted mode the agent's file tools reach only its
// working directory and the directories it is granted, so research has to be
// granted every root the selected units live in, not just the best match's.
func TestResearchReadDirsCoverEveryUnitRoot(t *testing.T) {
	units := []mapper.Unit{
		{Key: "doc:upload:a.pdf", Meta: map[string]any{mapper.MetaRoot: "/stage/docs"}},
		{Key: "module:x"}, // the repository
		{Key: "doc:upload:b.pdf", Meta: map[string]any{mapper.MetaRoot: "/stage/docs"}},
		{Key: "page:web:c", Meta: map[string]any{mapper.MetaRoot: "/stage/web"}},
	}
	workDir := researchWorkDir("/repo", units, nil)
	if workDir != "/stage/docs" {
		t.Fatalf("workDir = %q, want the best match's root", workDir)
	}

	got := researchReadDirs("/repo", units, workDir)
	want := []string{"/repo", "/stage/web"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("researchReadDirs = %v, want %v (no duplicates, never the working directory)", got, want)
	}

	if got := researchReadDirs("", nil, ""); len(got) != 0 {
		t.Errorf("a run with no repository and no units granted %v", got)
	}
}
