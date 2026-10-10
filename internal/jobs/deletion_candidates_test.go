package jobs

import (
	"fmt"
	"testing"

	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// TestDeletionCandidatesDescribeEachSourceAgainstTheOthers pins the detail a
// reviewer reads: what approving each vanished source would remove, and what
// it would only regenerate because another source still claims it. Candidates
// share one reference count, so this also guards that one candidate's claims
// are not counted against another's.
func TestDeletionCandidatesDescribeEachSourceAgainstTheOthers(t *testing.T) {
	sources := []diff.SourceRecord{
		{Key: "module:a", FilesWritten: []string{"entities/a.md", "concepts/shared.md"}},
		{Key: "module:b", FilesWritten: []string{"entities/b.md", "concepts/shared.md"}},
		{Key: "module:kept", FilesWritten: []string{"concepts/shared.md", "entities/kept.md"}},
		{Key: "module:lonely", FilesWritten: []string{"entities/lonely.md"}},
	}
	m := &mapper.WorkspaceMap{Units: []mapper.Unit{{Key: "module:kept"}}}

	got := deletionCandidates(sources, m, nil, nil, nil)

	want := map[diff.Key]string{
		"module:a": "Source module:a is no longer present in the map. Approving removes 1 page(s): entities/a.md." +
			" 1 shared page(s) would be regenerated, not removed.",
		"module:b": "Source module:b is no longer present in the map. Approving removes 1 page(s): entities/b.md." +
			" 1 shared page(s) would be regenerated, not removed.",
		"module:lonely": "Source module:lonely is no longer present in the map. Approving removes 1 page(s): entities/lonely.md.",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want %d", got, len(want))
	}
	for _, c := range got {
		if w, ok := want[c.Key]; !ok || c.Detail != w {
			t.Errorf("candidate %s detail = %q, want %q", c.Key, c.Detail, w)
		}
	}
	// Sorted by key, so the review queue fills in a stable order.
	for i := 1; i < len(got); i++ {
		if got[i-1].Key >= got[i].Key {
			t.Errorf("candidates not sorted: %s before %s", got[i-1].Key, got[i].Key)
		}
	}
}

// BenchmarkDeletionCandidates is the namespace-vanishes case: 2,000 sources on
// record, none of them still mapped, each of which needs its consequence
// worked out.
func BenchmarkDeletionCandidates(b *testing.B) {
	sources := make([]diff.SourceRecord, 2000)
	for i := range sources {
		files := make([]string, 20)
		for j := range files {
			files[j] = fmt.Sprintf("concepts/page-%d.md", (i*7+j)%5000)
		}
		sources[i] = diff.SourceRecord{Key: diff.Key(fmt.Sprintf("module:m%04d", i)), FilesWritten: files}
	}
	// One surviving unit of the same kind, so the module namespace counts as
	// synced and every other module is a candidate.
	m := &mapper.WorkspaceMap{Units: []mapper.Unit{{Key: "module:survivor"}}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := deletionCandidates(sources, m, nil, nil, nil); len(got) != len(sources) {
			b.Fatalf("candidates = %d, want %d", len(got), len(sources))
		}
	}
}
