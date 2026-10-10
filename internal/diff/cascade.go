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
	// ConnectorID names the connector whose sync produced this source, empty
	// for CLI builds. Written exactly as-is on import — including empty — so
	// a connector swap or a CLI rebuild never leaves stale attribution.
	ConnectorID string
	// NeedsRegen marks a source whose pages survived a cascade but still
	// describe a source that has since been deleted. Set by the deletion that
	// spared the page and cleared by the run that rewrites it, so the intent
	// survives between the two — unlike RegeneratePages, which only exists for
	// the duration of the run that plans the cascade.
	NeedsRegen bool
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
	return NewCascadePlanner(all).Plan(removed)
}

// CascadePlanner plans several cascades against the same set of sources.
//
// PlanCascade reference counts every page and blob on every call, which is
// O(sources x files). That is fine for one deletion and quadratic for a review
// per vanished source, so the planner counts once and derives each plan by
// subtracting the removed sources' own claims.
type CascadePlanner struct {
	byKey map[Key][]SourceRecord
	pages map[string]int
	blobs map[string]int
}

// NewCascadePlanner reference counts the pages and blobs claimed by all.
func NewCascadePlanner(all []SourceRecord) *CascadePlanner {
	p := &CascadePlanner{
		byKey: make(map[Key][]SourceRecord, len(all)),
		pages: map[string]int{},
		blobs: map[string]int{},
	}
	for _, s := range all {
		p.byKey[s.Key] = append(p.byKey[s.Key], s)
		for _, f := range s.FilesWritten {
			p.pages[f]++
		}
		for _, b := range s.BlobKeys {
			p.blobs[b]++
		}
	}
	return p
}

// Plan is PlanCascade for the planner's sources.
func (p *CascadePlanner) Plan(removed []Key) Cascade {
	removing := make(map[Key]bool, len(removed))
	for _, k := range removed {
		removing[k] = true
	}

	// What the surviving sources still claim is what everyone claims, less the
	// removed sources' own claims.
	leavingPages := map[string]int{}
	leavingBlobs := map[string]int{}
	for k := range removing {
		for _, s := range p.byKey[k] {
			for _, f := range s.FilesWritten {
				leavingPages[f]++
			}
			for _, b := range s.BlobKeys {
				leavingBlobs[b]++
			}
		}
	}

	deletePages := map[string]bool{}
	regenerate := map[string]bool{}
	deleteBlobs := map[string]bool{}

	for k := range removing {
		for _, s := range p.byKey[k] {
			for _, f := range s.FilesWritten {
				if p.pages[f]-leavingPages[f] > 0 {
					regenerate[f] = true
				} else {
					deletePages[f] = true
				}
			}
			for _, b := range s.BlobKeys {
				if p.blobs[b]-leavingBlobs[b] == 0 {
					deleteBlobs[b] = true
				}
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
