package wiki

import (
	"strings"
	"testing"
)

// goodBody is long enough to clear MinBodyBytes and has a heading.
const goodBody = `# Task Dispatch

Dispatch coordinates work across the ripple and beacon services, fanning tasks
out to workers and collecting results. It exists because the two services need
a shared notion of ownership.
`

func validPage() *Page {
	return &Page{
		Path: "concepts/task-dispatch.md",
		Slug: "task-dispatch",
		Meta: Frontmatter{
			Type: TypeConcept, Title: "Task Dispatch",
			Created: "2026-07-20", Updated: "2026-07-25",
		},
		Body: goodBody,
	}
}

func TestValidatePageAccepts(t *testing.T) {
	opts := ValidateOptions{RequireDates: true, KnownSlugs: map[string]bool{}}
	if v := ValidatePage(validPage(), opts); len(v) != 0 {
		t.Errorf("valid page rejected: %v", v)
	}
}

func TestValidatePageRejectsPathEscapes(t *testing.T) {
	// The path check runs before anything reads a file, so these must be caught
	// structurally rather than relying on the sandbox alone.
	paths := []string{
		"../outside.md",
		"../../etc/passwd.md",
		"/absolute/path.md",
		`concepts\windows.md`,
		"concepts/../../escape.md",
		"concepts/nested/too-deep.md",
		"unknown-dir/page.md",
		"toplevel.md",
		"concepts/page.txt",
		"",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			page := validPage()
			page.Path = p
			if v := ValidatePage(page, ValidateOptions{}); len(v) == 0 {
				t.Errorf("path %q was accepted", p)
			}
		})
	}
}

func TestValidatePageRejectsReserved(t *testing.T) {
	// index, overview, and log are derived from page frontmatter. An agent
	// writing them would let navigation drift from content.
	for _, p := range ReservedPages {
		t.Run(p, func(t *testing.T) {
			page := validPage()
			page.Path = p
			page.Slug = SlugFromPath(p)

			v := ValidatePage(page, ValidateOptions{})
			if len(v) == 0 {
				t.Fatalf("%s was accepted", p)
			}
			if !strings.Contains(v[0].Reason, "maintained by kiln") {
				t.Errorf("reason should explain why: %q", v[0].Reason)
			}
		})
	}
}

func TestValidatePageRejectsTypeDirectoryMismatch(t *testing.T) {
	page := validPage()
	page.Path = "entities/task-dispatch.md" // type says concept

	v := ValidatePage(page, ValidateOptions{})
	if len(v) == 0 {
		t.Fatal("type/directory mismatch was accepted")
	}
	if !strings.Contains(v[0].Reason, "does not match directory") {
		t.Errorf("unexpected reason: %q", v[0].Reason)
	}
}

func TestValidatePageRejectsUnwritableTypes(t *testing.T) {
	page := validPage()
	page.Path = "overview.md"
	page.Meta.Type = TypeOverview

	if v := ValidatePage(page, ValidateOptions{}); len(v) == 0 {
		t.Error("an agent-written overview page was accepted")
	}
}

func TestValidatePageRejectsStubs(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace only", "   \n\n  "},
		{"too short", "# X\n\nShort.\n"},
		{"no heading", strings.Repeat("Prose without any heading at all. ", 8)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := validPage()
			page.Body = tt.body
			if v := ValidatePage(page, ValidateOptions{}); len(v) == 0 {
				t.Errorf("body %q was accepted", tt.body)
			}
		})
	}
}

func TestValidatePageRejectsLeftoverMarkers(t *testing.T) {
	page := validPage()
	page.Body = goodBody + "\n---FILE: concepts/other.md---\n"

	v := ValidatePage(page, ValidateOptions{})
	if len(v) == 0 {
		t.Fatal("leftover block markers were accepted")
	}
}

func TestValidatePageEnforcesPlan(t *testing.T) {
	page := validPage()
	opts := ValidateOptions{Planned: map[string]bool{"concepts/something-else.md": true}}

	v := ValidatePage(page, opts)
	if len(v) == 0 {
		t.Fatal("an unplanned page was accepted")
	}
	if !strings.Contains(v[0].Reason, "approved plan") {
		t.Errorf("unexpected reason: %q", v[0].Reason)
	}
}

func TestValidatePageUnresolvedLinkTolerance(t *testing.T) {
	page := validPage()
	page.Body = goodBody + "\nSee [[a]], [[b]], [[c]], [[d]], [[e]].\n"

	// A few dangling links are pruned and reported; many mean the agent
	// invented a structure that does not exist.
	opts := ValidateOptions{KnownSlugs: map[string]bool{}, MaxUnresolvedLinks: 3}
	if v := ValidatePage(page, opts); len(v) == 0 {
		t.Error("five unresolved links were accepted with a limit of three")
	}

	opts.MaxUnresolvedLinks = 10
	if v := ValidatePage(page, opts); len(v) != 0 {
		t.Errorf("five unresolved links rejected with a limit of ten: %v", v)
	}
}

func TestValidatePageReportsAllViolations(t *testing.T) {
	page := &Page{
		Path: "concepts/Bad_Slug.md",
		Slug: "Bad_Slug",
		Meta: Frontmatter{Type: TypeConcept, Title: "", Created: "nope", Updated: ""},
		Body: "x",
	}

	v := ValidatePage(page, ValidateOptions{RequireDates: true})
	// One corrective turn should be able to fix everything, so all problems
	// must be reported together.
	if len(v) < 4 {
		t.Errorf("got %d violations, want at least 4 (slug, title, dates, body):\n%v", len(v), v)
	}
}

func TestValidateBatchResolvesWithinBatch(t *testing.T) {
	a := validPage()
	a.Body = goodBody + "\nRelated: [[sibling-page]].\n"

	b := &Page{
		Path: "concepts/sibling-page.md",
		Slug: "sibling-page",
		Meta: Frontmatter{Type: TypeConcept, Title: "Sibling", Created: "2026-07-20", Updated: "2026-07-25"},
		Body: goodBody,
	}

	// A link to a page written in the same run must resolve, or every batch
	// with internal cross-references would fail.
	opts := ValidateOptions{KnownSlugs: map[string]bool{}, MaxUnresolvedLinks: 0}
	if v := ValidateBatch([]*Page{a, b}, opts); len(v) != 0 {
		t.Errorf("intra-batch link treated as unresolved: %v", v)
	}
}

func TestValidateBatchRejectsDuplicatePaths(t *testing.T) {
	a := validPage()
	b := validPage()

	v := ValidateBatch([]*Page{a, b}, ValidateOptions{})
	if len(v) == 0 {
		t.Fatal("the same path written twice was accepted")
	}
	if !strings.Contains(v[0].Reason, "more than once") {
		t.Errorf("unexpected reason: %q", v[0].Reason)
	}
}

func TestViolationString(t *testing.T) {
	v := Violation{Path: "concepts/x.md", Reason: "body is empty"}
	if got, want := v.String(), "concepts/x.md: body is empty"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	v = Violation{Reason: "run-level problem"}
	if got, want := v.String(), "run-level problem"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
