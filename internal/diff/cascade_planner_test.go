package diff

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// planCascadeByRecount is the straightforward definition the planner must
// agree with: reference count the sources that are not being removed, from
// scratch, for every call.
func planCascadeByRecount(all []SourceRecord, removed []Key) Cascade {
	removing := make(map[Key]bool, len(removed))
	for _, k := range removed {
		removing[k] = true
	}
	survivingPages := map[string]int{}
	survivingBlobs := map[string]int{}
	for _, s := range all {
		if removing[s.Key] {
			continue
		}
		for _, p := range s.FilesWritten {
			survivingPages[p]++
		}
		for _, b := range s.BlobKeys {
			survivingBlobs[b]++
		}
	}
	deletePages := map[string]bool{}
	regenerate := map[string]bool{}
	deleteBlobs := map[string]bool{}
	for _, s := range all {
		if !removing[s.Key] {
			continue
		}
		for _, p := range s.FilesWritten {
			if survivingPages[p] > 0 {
				regenerate[p] = true
			} else {
				deletePages[p] = true
			}
		}
		for _, b := range s.BlobKeys {
			if survivingBlobs[b] == 0 {
				deleteBlobs[b] = true
			}
		}
	}
	return Cascade{
		DeletePages:     sortedKeys(deletePages),
		RegeneratePages: sortedKeys(regenerate),
		DeleteBlobs:     sortedKeys(deleteBlobs),
		DropSources:     sortedSourceKeys(removing),
	}
}

// TestCascadePlannerAgreesWithRecounting checks the planner's subtract-from-
// the-total shortcut against recounting, over random overlapping claims --
// including repeated files within a source, repeated keys, and removed keys
// that are not on record.
func TestCascadePlannerAgreesWithRecounting(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pick := func(prefix string, n int) []string {
		out := make([]string, rng.Intn(5))
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, rng.Intn(n))
		}
		return out
	}

	for round := 0; round < 200; round++ {
		sources := make([]SourceRecord, rng.Intn(12))
		for i := range sources {
			// A small key space makes repeated keys likely.
			sources[i] = SourceRecord{
				Key:          ModuleKey(fmt.Sprintf("m%d", rng.Intn(10))),
				FilesWritten: pick("page", 8),
				BlobKeys:     pick("blob", 5),
			}
		}
		planner := NewCascadePlanner(sources)

		// Several plans from one planner, as a deletion review pass does.
		for plan := 0; plan < 6; plan++ {
			removed := make([]Key, rng.Intn(4))
			for i := range removed {
				removed[i] = ModuleKey(fmt.Sprintf("m%d", rng.Intn(12)))
			}
			want := planCascadeByRecount(sources, removed)
			if got := planner.Plan(removed); !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d: removing %v from %+v\n got %+v\nwant %+v", round, removed, sources, got, want)
			}
			if got := PlanCascade(sources, removed); !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d: PlanCascade diverged\n got %+v\nwant %+v", round, got, want)
			}
		}
	}
}

// BenchmarkPlanEachSource shows what the planner is for: one plan per source,
// 2,000 sources each claiming 20 pages.
func BenchmarkPlanEachSource(b *testing.B) {
	sources := make([]SourceRecord, 2000)
	for i := range sources {
		files := make([]string, 20)
		for j := range files {
			files[j] = fmt.Sprintf("page-%d", (i*7+j)%5000)
		}
		sources[i] = SourceRecord{Key: DocKey(fmt.Sprintf("d%d", i)), FilesWritten: files}
	}

	b.Run("recount", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, s := range sources {
				_ = planCascadeByRecount(sources, []Key{s.Key})
			}
		}
	})
	b.Run("planner", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			p := NewCascadePlanner(sources)
			for _, s := range sources {
				_ = p.Plan([]Key{s.Key})
			}
		}
	})
}
