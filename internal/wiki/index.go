package wiki

import (
	"fmt"
	"sort"
	"strings"
)

// IndexSections is the fixed section order. Every section is emitted even when
// empty, so the file's shape is stable and a diff shows only real changes.
var IndexSections = []PageType{
	TypeEntity, TypeConcept, TypeSource, TypeQuery, TypeComparison, TypeSynthesis,
}

// IndexEntry is one catalog line.
type IndexEntry struct {
	Type    PageType
	Slug    string
	Title   string
	Updated string
}

// IndexOptions controls index rendering.
type IndexOptions struct {
	// RecentLimit sizes the "Recently Updated" tail. Zero omits the section.
	RecentLimit int
}

// DefaultRecentLimit matches the observed llm_wiki behavior closely enough to
// feel familiar without being tuned to it.
const DefaultRecentLimit = 15

// BuildIndex renders index.md from page frontmatter.
//
// The index is regenerated wholesale on every run and never patched, and the
// LLM is forbidden from writing it. An LLM-maintained catalog drifts out of
// sync with the pages it describes; a derived one cannot.
func BuildIndex(pages []Page, opts IndexOptions) string {
	if opts.RecentLimit == 0 {
		opts.RecentLimit = DefaultRecentLimit
	}

	byType := make(map[PageType][]IndexEntry, len(IndexSections))
	var all []IndexEntry

	for _, p := range pages {
		if p.Meta.Type == TypeOverview || IsReserved(p.Path) {
			continue
		}
		e := IndexEntry{
			Type:    p.Meta.Type,
			Slug:    p.Slug,
			Title:   p.Meta.Title,
			Updated: p.Meta.Updated,
		}
		byType[p.Meta.Type] = append(byType[p.Meta.Type], e)
		all = append(all, e)
	}

	var b strings.Builder
	b.WriteString("# Wiki Index\n")

	for _, t := range IndexSections {
		fmt.Fprintf(&b, "\n## %s\n", sectionHeading(t))
		entries := byType[t]
		sortEntriesByTitle(entries)
		for _, e := range entries {
			fmt.Fprintf(&b, "- %s\n", indexLink(e))
		}
	}

	if opts.RecentLimit > 0 && len(all) > 0 {
		b.WriteString("\n## Recently Updated\n")
		for _, e := range recentEntries(all, opts.RecentLimit) {
			fmt.Fprintf(&b, "- %s\n", indexLink(e))
		}
	}

	return b.String()
}

// indexLink renders the "[[dir/slug|Title]]" form. The alias is omitted when it
// would merely repeat the slug.
func indexLink(e IndexEntry) string {
	target := fmt.Sprintf("%s/%s", e.Type.Dir(), e.Slug)
	if e.Title == "" || e.Title == e.Slug {
		return fmt.Sprintf("[[%s]]", target)
	}
	return fmt.Sprintf("[[%s|%s]]", target, e.Title)
}

func sectionHeading(t PageType) string {
	switch t {
	case TypeEntity:
		return "Entities"
	case TypeConcept:
		return "Concepts"
	case TypeSource:
		return "Sources"
	case TypeQuery:
		return "Queries"
	case TypeComparison:
		return "Comparisons"
	case TypeSynthesis:
		return "Synthesis"
	default:
		return strings.ToUpper(string(t)[:1]) + string(t)[1:]
	}
}

// sortEntriesByTitle orders case-insensitively, falling back to slug so the
// output is fully deterministic even when two pages share a title.
func sortEntriesByTitle(entries []IndexEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		li := strings.ToLower(entries[i].Title)
		lj := strings.ToLower(entries[j].Title)
		if li != lj {
			return li < lj
		}
		return entries[i].Slug < entries[j].Slug
	})
}

// recentEntries returns the most recently updated entries, newest first.
func recentEntries(all []IndexEntry, limit int) []IndexEntry {
	sorted := make([]IndexEntry, len(all))
	copy(sorted, all)

	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Updated != sorted[j].Updated {
			return sorted[i].Updated > sorted[j].Updated
		}
		return sorted[i].Slug < sorted[j].Slug
	})

	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	return sorted
}
