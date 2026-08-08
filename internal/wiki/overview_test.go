package wiki

import (
	"strings"
	"testing"
)

func TestOverviewDescribesItsSources(t *testing.T) {
	got := BuildOverview(samplePages(), OverviewInput{
		Workspace: "watchtower",
		Date:      "2026-07-25",
		Sources: []SourceTally{
			{Kind: "module", Count: 5},
			{Kind: "doc:upload", Count: 1},
		},
	})

	// The lede answers "what is this made of" before any page count does.
	if !strings.Contains(got, "compiled from 5 code modules and 1 uploaded document") {
		t.Errorf("lede does not describe the sources:\n%s", got)
	}
	if !strings.Contains(got, "## What this bench reads") {
		t.Errorf("no sources section:\n%s", got)
	}
}

func TestOverviewCountsReadAsEnglish(t *testing.T) {
	// One of a thing is not "1 entities". The counts are the most-read line in
	// the file and the plural was wrong for every singleton type.
	got := BuildOverview(samplePages(), OverviewInput{Workspace: "w", Date: "2026-07-25"})

	for _, want := range []string{"1 source", "1 query", "1 comparison", "1 synthesis page"} {
		if !strings.Contains(got, "- "+want+"\n") {
			t.Errorf("missing %q in counts:\n%s", want, got)
		}
	}
	for _, bad := range []string{"1 sources", "1 queries", "1 comparisons"} {
		if strings.Contains(got, bad) {
			t.Errorf("counts read %q:\n%s", bad, got)
		}
	}
	// Plurals still pluralize.
	if !strings.Contains(got, "- 3 entities\n") {
		t.Errorf("plural count missing:\n%s", got)
	}
}

func TestOverviewDescribesSourcesBeforeAnyPagesExist(t *testing.T) {
	// A source connected but not yet built is invisible in a page count. That
	// is precisely the state a reader would be confused by, so the overview
	// still names what the bench reads.
	got := BuildOverview(nil, OverviewInput{
		Workspace: "fresh",
		Date:      "2026-07-25",
		Sources:   []SourceTally{{Kind: "doc:web", Count: 2}},
	})

	if !strings.Contains(got, "2 fetched web pages") {
		t.Errorf("sources missing from a bench with no pages:\n%s", got)
	}
	if !strings.Contains(got, "no pages yet") {
		t.Errorf("empty wiki should still say so:\n%s", got)
	}
	if _, _, err := ParseFrontmatter([]byte(got)); err != nil {
		t.Errorf("invalid frontmatter: %v", err)
	}
}

func TestOverviewIgnoresTheSynthesisUnitAsASource(t *testing.T) {
	// arch:overview is derived from the other material rather than being
	// material; counting it would inflate every bench by one.
	got := BuildOverview(samplePages(), OverviewInput{
		Workspace: "w",
		Date:      "2026-07-25",
		Sources:   []SourceTally{{Kind: "module", Count: 2}, {Kind: "arch", Count: 1}},
	})

	if !strings.Contains(got, "compiled from 2 code modules.") {
		t.Errorf("arch unit leaked into the source list:\n%s", got)
	}
}

func TestOverviewWithSourcesIsDeterministic(t *testing.T) {
	pages := samplePages()
	in := OverviewInput{
		Workspace: "w",
		Date:      "2026-07-25",
		Sources:   []SourceTally{{Kind: "module", Count: 2}, {Kind: "doc:web", Count: 1}},
	}

	first := BuildOverview(pages, in)
	for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
		pages[i], pages[j] = pages[j], pages[i]
	}
	if second := BuildOverview(pages, in); first != second {
		t.Error("overview varies with page order")
	}
}

func TestJoinPhrases(t *testing.T) {
	cases := map[string][]string{
		"":               {},
		"a":              {"a"},
		"a and b":        {"a", "b"},
		"a, b, and c":    {"a", "b", "c"},
		"a, b, c, and d": {"a", "b", "c", "d"},
	}
	for want, in := range cases {
		if got := joinPhrases(in); got != want {
			t.Errorf("joinPhrases(%v) = %q, want %q", in, got, want)
		}
	}
}
