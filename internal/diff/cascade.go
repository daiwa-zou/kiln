package diff

import "sort"

// SourceRecord is the persisted state of one source: what it hashed to and
// which pages it produced. FilesWritten is what makes cascade deletion
// possible at all.
type SourceRecord struct {
	Key          Key
	InputHash    string
	FilesWritten []string
	BlobKeys     []string
}

// Cascade is the outcome of removing a set of sources.
type Cascade struct {
	// DeletePages are pages no remaining live source claims.
	DeletePages []string
	// RegeneratePages are shared pages that survive but whose prose still
	// describes the departed source.
	RegeneratePages []string
	// DeleteBlobs are blobs no remaining source references. Consumed by the
	// object-storage subsystem when it lands; until then the field is data,
	// computed and tested but with no side effect.
	DeleteBlobs []string
	// DropSources are the source keys to remove.
	DropSources []Key
}

// PlanCascade works out what removing `removed` implies, given every source
// currently on record.
//
// Two rules matter here. A page claimed by another live source is kept, or
// deleting one of three modules would destroy a concept page derived from all
// three. And a kept page is marked for regeneration rather than merely having
// its frontmatter amended -- its body still describes material that no longer
// exists, and leaving that is how a wiki accumulates confident claims about
// deleted code.
func PlanCascade(all []SourceRecord, removed []Key) Cascade {
	removing := make(map[Key]bool, len(removed))
	for _, k := range removed {
		removing[k] = true
	}

	// Reference count pages and blobs across the sources that survive.
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

// Empty reports whether the cascade would change anything.
func (c Cascade) Empty() bool {
	return len(c.DeletePages) == 0 && len(c.RegeneratePages) == 0 &&
		len(c.DeleteBlobs) == 0 && len(c.DropSources) == 0
}

// PageCount is how many pages would be removed, which is the number a deletion
// review shows the user before they confirm.
func (c Cascade) PageCount() int { return len(c.DeletePages) }

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSourceKeys(m map[Key]bool) []Key {
	if len(m) == 0 {
		return nil
	}
	out := make([]Key, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
