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
	// A bench with uploads says what its document count covers, so the lede
	// does not contradict an Ingest list that has grown since the build.
	if !strings.Contains(got, "last built 2026-07-25; documents uploaded after this build are not counted.") {
		t.Errorf("lede does not say what the document count covers:\n%s", got)
	}
	if !strings.Contains(got, "## What this bench reads") {
		t.Errorf("no sources section:\n%s", got)
	}
}

func TestOverviewLedeQualifiesTheDocumentCountOnlyWithUploads(t *testing.T) {
	// Overview said "25 uploaded documents" while Ingest listed 26, because the
	// tally is what the build read and a document uploaded since is not in it.
	// The clause reconciles the two, and only where there is something to
	// reconcile: a git-only bench keeps its plain full stop.
	const clause = "documents uploaded after this build are not counted."

	gitOnly := BuildOverview(samplePages(), OverviewInput{
		Workspace: "w",
		Date:      "2026-07-25",
		BuiltAt:   "2026-07-25 09:42 UTC",
		Sources:   []SourceTally{{Kind: "module", Count: 2}},
	})
	if strings.Contains(gitOnly, clause) {
		t.Errorf("git-only bench has no document count to qualify:\n%s", gitOnly)
	}
	if !strings.Contains(gitOnly, "last built 2026-07-25 09:42 UTC.") {
		t.Errorf("git-only bench lost its plain stop:\n%s", gitOnly)
	}

	withUploads := BuildOverview(samplePages(), OverviewInput{
		Workspace: "w",
		Date:      "2026-07-25",
		BuiltAt:   "2026-07-25 09:42 UTC",
		Sources:   []SourceTally{{Kind: "module", Count: 2}, {Kind: "doc:upload", Count: 3}},
	})
	if !strings.Contains(withUploads, "last built 2026-07-25 09:42 UTC; "+clause) {
		t.Errorf("clause missing or detached from the build stamp:\n%s", withUploads)
	}

	// The clause rides on whichever build sentence the lede has: a bench whose
	// uploads produced no pages yet still gets it after "Last built".
	noPages := BuildOverview(nil, OverviewInput{
		Workspace: "w",
		Date:      "2026-07-25",
		Sources:   []SourceTally{{Kind: "doc:upload", Count: 1}},
	})
	if !strings.Contains(noPages, "Last built 2026-07-25; "+clause) {
		t.Errorf("clause missing on a bench with no pages:\n%s", noPages)
	}
}

// When the build skipped an upload, the gap between the lede's count and the
// Ingest list is that document, not a later upload, so the lede says so: how
// many of the documents it was handed went in, and that the rest could not be
// read. The after-this-build clause stays, because it is also still true.
func TestOverviewLedeSaysWhatCouldNotBeRead(t *testing.T) {
	skipped := BuildOverview(samplePages(), OverviewInput{
		Workspace:      "w",
		Date:           "2026-07-25",
		BuiltAt:        "2026-07-25 09:42 UTC",
		Sources:        []SourceTally{{Kind: "doc:upload", Count: 27}},
		UploadsStaged:  28,
		UploadsSkipped: 1,
	})
	want := "compiled from 27 of 28 uploaded documents; 1 could not be read (see Ingest). " +
		"9 pages, last built 2026-07-25 09:42 UTC; documents uploaded after this build are not counted."
	if !strings.Contains(skipped, want) {
		t.Errorf("lede does not account for the skipped document:\n%s", skipped)
	}

	// The phrase is about documents, so it comes from the build's counts even
	// where the tally (which counts a long document's sections too) differs.
	sectioned := BuildOverview(samplePages(), OverviewInput{
		Workspace:      "w",
		Date:           "2026-07-25",
		Sources:        []SourceTally{{Kind: "module", Count: 2}, {Kind: "doc:upload", Count: 31}},
		UploadsStaged:  28,
		UploadsSkipped: 2,
	})
	if !strings.Contains(sectioned, "compiled from 2 code modules and 26 of 28 uploaded documents; 2 could not be read (see Ingest).") {
		t.Errorf("lede counts units rather than documents:\n%s", sectioned)
	}

	// A bench whose only upload was skipped has no upload tally at all, and
	// the lede still says what happened to it.
	allSkipped := BuildOverview(nil, OverviewInput{
		Workspace:      "w",
		Date:           "2026-07-25",
		UploadsStaged:  1,
		UploadsSkipped: 1,
	})
	if !strings.Contains(allSkipped, "compiled from 0 of 1 uploaded document; 1 could not be read (see Ingest). "+
		"Last built 2026-07-25; documents uploaded after this build are not counted.") {
		t.Errorf("lede is silent about a bench whose one upload was skipped:\n%s", allSkipped)
	}

	// Nothing skipped: the plain tally, as before.
	plain := BuildOverview(samplePages(), OverviewInput{
		Workspace:     "w",
		Date:          "2026-07-25",
		Sources:       []SourceTally{{Kind: "doc:upload", Count: 27}},
		UploadsStaged: 27,
	})
	if !strings.Contains(plain, "compiled from 27 uploaded documents. 9 pages, last built 2026-07-25; documents uploaded after this build are not counted.") {
		t.Errorf("a build that skipped nothing changed the lede:\n%s", plain)
	}
	if strings.Contains(plain, "could not be read") {
		t.Errorf("a build that skipped nothing claims a skip:\n%s", plain)
	}
}

func TestOverviewLedeReportsBuildTimeToTheMinute(t *testing.T) {
	// A bench rebuilt three times in a morning said "last built" the same way
	// all three times, which is the one question the line exists to answer.
	got := BuildOverview(samplePages(), OverviewInput{
		Workspace: "watchtower",
		Date:      "2026-07-25",
		BuiltAt:   "2026-07-25 09:42 UTC",
	})
	if !strings.Contains(got, "last built 2026-07-25 09:42 UTC.") {
		t.Errorf("lede does not carry the clock:\n%s", got)
	}
	// The frontmatter keeps the calendar day: a page regenerated twice in one
	// day should still not look like it changed.
	if !strings.Contains(got, "updated: 2026-07-25\n") {
		t.Errorf("frontmatter should stay a calendar day:\n%s", got)
	}
}

func TestOverviewLedeFallsBackToTheDate(t *testing.T) {
	// A caller that has not been taught about BuiltAt still renders a sentence
	// rather than "last built ." with the stamp missing.
	got := BuildOverview(samplePages(), OverviewInput{Workspace: "w", Date: "2026-07-25"})
	if !strings.Contains(got, "last built 2026-07-25.") {
		t.Errorf("no fallback to the date:\n%s", got)
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
