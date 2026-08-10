package wiki

import (
	"fmt"
	"sort"
	"strings"
)

// OverviewInput is the deterministic frame an overview is built from.
type OverviewInput struct {
	Workspace string
	// Ref is the source revision the wiki reflects, so a reader can tell how
	// current it is without comparing timestamps.
	Ref string
	// Narrative is the agent's findings, slotted into the generated frame. The
	// agent never writes overview.md itself -- it contributes prose and the
	// structure around it stays derived.
	Narrative []string
	Date      string
	// BuiltAt is when the build ran, to the minute. Date still stamps the
	// frontmatter, where a calendar day is deliberate; the lede reports this
	// instead, because "last built today" is not an answer to anyone deciding
	// whether to wait for a rebuild or read what is here. Empty falls back to
	// Date, so a caller that has not been taught the difference still renders.
	BuiltAt string
	// Sources tallies the material the wiki was compiled from. Without it the
	// overview could only describe the pages that came out, which says nothing
	// about what went in and does not change when a source is added that has
	// not yet produced a page.
	Sources []SourceTally
}

// SourceTally is one kind of material and how much of it fed the build.
type SourceTally struct {
	// Kind is a unit-key namespace: "module", "doc:upload", "doc:web", "doc",
	// or "arch". Phrasing lives here rather than at the call site so every
	// surface names a source the same way.
	Kind  string
	Count int
}

// sourcePhrase renders a tally as English, singular or plural.
func sourcePhrase(t SourceTally) string {
	one, many := "unit", "units"
	switch t.Kind {
	case "module":
		one, many = "code module", "code modules"
	case "doc:upload":
		one, many = "uploaded document", "uploaded documents"
	case "doc:web":
		one, many = "fetched web page", "fetched web pages"
	case "doc":
		one, many = "repository document", "repository documents"
	case "arch":
		// The synthesis unit is derived from everything else rather than being
		// material in its own right; it is never a source worth counting.
		return ""
	}
	if t.Count == 1 {
		return fmt.Sprintf("%d %s", t.Count, one)
	}
	return fmt.Sprintf("%d %s", t.Count, many)
}

// sectionNoun names one page of a type, for counts that can read "1 entity"
// rather than "1 entities".
func sectionNoun(t PageType, n int) string {
	if n == 1 {
		switch t {
		case TypeEntity:
			return "entity"
		case TypeConcept:
			return "concept"
		case TypeSource:
			return "source"
		case TypeQuery:
			return "query"
		case TypeComparison:
			return "comparison"
		case TypeSynthesis:
			return "synthesis page"
		}
	}
	return strings.ToLower(sectionHeading(t))
}

// joinPhrases renders a list as prose: "a", "a and b", "a, b, and c".
func joinPhrases(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
}

// BuildOverview renders overview.md from the current page set.
//
// Like the index, this is regenerated wholesale on every run and is never
// agent-writable. An LLM-maintained overview drifts out of step with the pages
// it claims to summarize; a derived one cannot.
func BuildOverview(pages []Page, in OverviewInput) string {
	var b strings.Builder

	b.WriteString(Frontmatter{
		Type:       TypeOverview,
		Title:      "Overview",
		Updated:    in.Date,
		BuiltAtRef: in.Ref,
	}.Render())
	b.WriteString("\n# Overview\n\n")

	writeOverviewLede(&b, pages, in)

	if len(in.Narrative) > 0 {
		for _, para := range in.Narrative {
			if strings.TrimSpace(para) == "" {
				continue
			}
			b.WriteString(para)
			b.WriteString("\n\n")
		}
	}

	writeOverviewSources(&b, in)
	writeOverviewCounts(&b, pages)
	writeOverviewEntry(&b, pages)

	return b.String()
}

// writeOverviewLede says what this bench is in one sentence: what it is called,
// what it was compiled from, and when. A reader landing here cold gets the
// shape of the thing before any of its contents, and the sentence changes on
// its own as sources are connected and pages accumulate.
func writeOverviewLede(b *strings.Builder, pages []Page, in OverviewInput) {
	name := in.Workspace
	if name == "" {
		name = "this bench"
	} else {
		name = "**" + in.Workspace + "**"
	}

	var parts []string
	for _, t := range in.Sources {
		if p := sourcePhrase(t); p != "" && t.Count > 0 {
			parts = append(parts, p)
		}
	}

	if len(parts) > 0 {
		fmt.Fprintf(b, "Knowledge base for %s, compiled from %s", name, joinPhrases(parts))
	} else {
		fmt.Fprintf(b, "Knowledge base for %s", name)
	}
	if in.Ref != "" {
		fmt.Fprintf(b, ", at `%s`", in.Ref)
	}
	b.WriteString(".")

	stamp := in.BuiltAt
	if stamp == "" {
		stamp = in.Date
	}
	if n := countedPages(pages); n > 0 && stamp != "" {
		fmt.Fprintf(b, " %s, last built %s.", plural(n, "page"), stamp)
	} else if stamp != "" {
		fmt.Fprintf(b, " Last built %s.", stamp)
	}
	b.WriteString("\n\n")
}

// writeOverviewSources lists the material behind the wiki. It is deliberately
// separate from the page counts: a source that has been connected but has not
// produced pages yet is invisible in a page count, and its absence there is
// exactly the thing a reader would be confused by.
func writeOverviewSources(b *strings.Builder, in OverviewInput) {
	var lines []string
	for _, t := range in.Sources {
		if p := sourcePhrase(t); p != "" && t.Count > 0 {
			lines = append(lines, p)
		}
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("## What this bench reads\n\n")
	for _, l := range lines {
		fmt.Fprintf(b, "- %s\n", l)
	}
	b.WriteString("\n")
}

// countedPages is the number of real pages: the derived artifacts describe the
// wiki rather than being part of it.
func countedPages(pages []Page) int {
	n := 0
	for _, p := range pages {
		if p.Meta.Type == TypeOverview || IsReserved(p.Path) {
			continue
		}
		n++
	}
	return n
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func writeOverviewCounts(b *strings.Builder, pages []Page) {
	counts := map[PageType]int{}
	for _, p := range pages {
		if p.Meta.Type == TypeOverview || IsReserved(p.Path) {
			continue
		}
		counts[p.Meta.Type]++
	}

	total := 0
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		b.WriteString("This wiki has no pages yet.\n")
		return
	}

	fmt.Fprintf(b, "## Contents\n\n%s:\n\n", plural(total, "page"))
	for _, t := range IndexSections {
		if n := counts[t]; n > 0 {
			fmt.Fprintf(b, "- %d %s\n", n, sectionNoun(t, n))
		}
	}
	b.WriteString("\n")
}

// writeOverviewEntry points a reader at the synthesis pages first, since those
// are the ones that explain the whole rather than a part.
func writeOverviewEntry(b *strings.Builder, pages []Page) {
	var synthesis []Page
	for _, p := range pages {
		if p.Meta.Type == TypeSynthesis {
			synthesis = append(synthesis, p)
		}
	}
	if len(synthesis) == 0 {
		return
	}

	sort.Slice(synthesis, func(i, j int) bool {
		return strings.ToLower(synthesis[i].Meta.Title) < strings.ToLower(synthesis[j].Meta.Title)
	})

	b.WriteString("## Start here\n\n")
	for _, p := range synthesis {
		title := p.Meta.Title
		if title == "" {
			title = p.Slug
		}
		fmt.Fprintf(b, "- [[%s/%s|%s]]\n", p.Meta.Type.Dir(), p.Slug, title)
	}
	b.WriteString("\n")
}
