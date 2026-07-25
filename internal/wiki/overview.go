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

	if in.Workspace != "" {
		fmt.Fprintf(&b, "Knowledge base for **%s**", in.Workspace)
		if in.Ref != "" {
			fmt.Fprintf(&b, ", built at `%s`", in.Ref)
		}
		b.WriteString(".\n\n")
	}

	if len(in.Narrative) > 0 {
		for _, para := range in.Narrative {
			if strings.TrimSpace(para) == "" {
				continue
			}
			b.WriteString(para)
			b.WriteString("\n\n")
		}
	}

	writeOverviewCounts(&b, pages)
	writeOverviewEntry(&b, pages)

	return b.String()
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

	fmt.Fprintf(b, "## Contents\n\n%d pages:\n\n", total)
	for _, t := range IndexSections {
		if n := counts[t]; n > 0 {
			fmt.Fprintf(b, "- %d %s\n", n, strings.ToLower(sectionHeading(t)))
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
