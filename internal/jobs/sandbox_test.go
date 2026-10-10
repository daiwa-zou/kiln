package jobs

import (
	"reflect"
	"testing"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

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
