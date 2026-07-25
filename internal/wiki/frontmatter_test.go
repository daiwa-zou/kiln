package wiki

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	raw := `---
type: concept
title: Aftermarket Cannibalization
created: 2026-07-20
updated: 2026-07-24
tags: [aftermarket, business-model]
related: [performance-indicator-llc, quality-signaling]
sources: ["PerformanceIndicator.pdf"]
---

# Aftermarket Cannibalization

Value-capture dynamic in which a durable good's secondary market competes
with the OEM's new-product sales.
`

	meta, body, err := ParseFrontmatter([]byte(raw))
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}

	if meta.Type != TypeConcept {
		t.Errorf("Type = %q, want concept", meta.Type)
	}
	if meta.Title != "Aftermarket Cannibalization" {
		t.Errorf("Title = %q", meta.Title)
	}
	if want := []string{"aftermarket", "business-model"}; !reflect.DeepEqual(meta.Tags, want) {
		t.Errorf("Tags = %v, want %v", meta.Tags, want)
	}
	if want := []string{"PerformanceIndicator.pdf"}; !reflect.DeepEqual(meta.Sources, want) {
		t.Errorf("Sources = %v, want %v", meta.Sources, want)
	}
	if !strings.HasPrefix(body, "# Aftermarket Cannibalization") {
		t.Errorf("body should start at the heading, got %q", body[:min(40, len(body))])
	}
}

func TestParseFrontmatterTolerances(t *testing.T) {
	// Agents produce these variations often enough that rejecting them would
	// cost a retry for no reason.
	tests := []struct {
		name string
		raw  string
	}{
		{"leading blank lines", "\n\n---\ntype: entity\ntitle: X\n---\n\n# X\n"},
		{"CRLF line endings", "---\r\ntype: entity\r\ntitle: X\r\n---\r\n\r\n# X\r\n"},
		{"UTF-8 BOM", "\ufeff---\ntype: entity\ntitle: X\n---\n\n# X\n"},
		{"no trailing newline", "---\ntype: entity\ntitle: X\n---\n# X"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, body, err := ParseFrontmatter([]byte(tt.raw))
			if err != nil {
				t.Fatalf("ParseFrontmatter: %v", err)
			}
			if meta.Type != TypeEntity || meta.Title != "X" {
				t.Errorf("meta = %+v, want type=entity title=X", meta)
			}
			if !strings.Contains(body, "# X") {
				t.Errorf("body = %q, want it to contain the heading", body)
			}
		})
	}
}

func TestParseFrontmatterErrors(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{"no frontmatter at all", "# Just a heading\n", ErrNoFrontmatter},
		{"body only", "some prose\n", ErrNoFrontmatter},
		{"empty input", "", ErrNoFrontmatter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ParseFrontmatter([]byte(tt.raw))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseFrontmatterUnterminated(t *testing.T) {
	_, _, err := ParseFrontmatter([]byte("---\ntype: entity\ntitle: X\n\n# no closing fence\n"))
	if err == nil {
		t.Fatal("accepted unterminated frontmatter")
	}
	if errors.Is(err, ErrNoFrontmatter) {
		t.Error("unterminated frontmatter should be distinct from missing frontmatter")
	}
}

func TestFrontmatterRoundTripIsStable(t *testing.T) {
	original := Frontmatter{
		Type:    TypeConcept,
		Title:   "Task Dispatch",
		Created: "2026-07-20",
		Updated: "2026-07-25",
		Tags:    []string{"queue", "scheduling"},
		Related: []string{"backpressure"},
		Sources: []string{"apps/ripple/go.mod"},
	}

	rendered := original.Render()
	parsed, _, err := ParseFrontmatter([]byte(rendered + "\n# Body\n"))
	if err != nil {
		t.Fatalf("ParseFrontmatter: %v", err)
	}

	if !reflect.DeepEqual(*parsed, original) {
		t.Errorf("round trip changed the value:\n got %+v\nwant %+v", *parsed, original)
	}

	// Re-rendering must be byte-identical, or every run would look like it
	// modified every page.
	if second := parsed.Render(); second != rendered {
		t.Errorf("render is not stable:\n--- first ---\n%s\n--- second ---\n%s", rendered, second)
	}
}

func TestFrontmatterQuotesAmbiguousTitles(t *testing.T) {
	// A title a YAML parser would read as a bool, number, or structure must
	// survive a round trip as text.
	titles := []string{"true", "No", "null", "3.14", "2026-07-25", "config: the sequel", "[brackets]", "#hash"}

	for _, title := range titles {
		t.Run(title, func(t *testing.T) {
			f := Frontmatter{Type: TypeEntity, Title: title}
			parsed, _, err := ParseFrontmatter([]byte(f.Render() + "\n# Body\n"))
			if err != nil {
				t.Fatalf("ParseFrontmatter(%q): %v", title, err)
			}
			if parsed.Title != title {
				t.Errorf("Title round-tripped %q as %q", title, parsed.Title)
			}
		})
	}
}

func TestFrontmatterOmitsEmptyFields(t *testing.T) {
	got := Frontmatter{Type: TypeEntity, Title: "X"}.Render()

	for _, absent := range []string{"tags:", "related:", "sources:", "provenance:", "built_at_ref:", "created:", "updated:"} {
		if strings.Contains(got, absent) {
			t.Errorf("rendered frontmatter should omit %q when empty:\n%s", absent, got)
		}
	}
}

func TestValidDate(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"2026-07-25", true},
		{"2026-02-29", false}, // 2026 is not a leap year
		{"2026-13-01", false},
		{"26-07-25", false},
		{"2026/07/25", false},
		{"", false},
		{"2026-07-25T00:00:00Z", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ValidDate(tt.in); got != tt.want {
				t.Errorf("ValidDate(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestPageTypeDirs(t *testing.T) {
	// Naive pluralization would produce entitys, querys, and synthesiss. These
	// directory names are part of the on-disk contract.
	want := map[PageType]string{
		TypeEntity:     "entities",
		TypeConcept:    "concepts",
		TypeSource:     "sources",
		TypeQuery:      "queries",
		TypeComparison: "comparisons",
		TypeSynthesis:  "synthesis",
	}

	for pt, dir := range want {
		if got := pt.Dir(); got != dir {
			t.Errorf("%s.Dir() = %q, want %q", pt, got, dir)
		}
		back, ok := TypeFromDir(dir)
		if !ok || back != pt {
			t.Errorf("TypeFromDir(%q) = %q, %v; want %q, true", dir, back, ok, pt)
		}
	}
}

func TestPageTypeGenerated(t *testing.T) {
	if TypeOverview.Generated() {
		t.Error("overview must not be agent-writable; it is emitted deterministically")
	}
	for _, pt := range GeneratedTypes {
		if !pt.Generated() {
			t.Errorf("%s should be agent-writable", pt)
		}
	}
	if PageType("nonsense").Valid() {
		t.Error("unknown type reported as valid")
	}
}

func TestPagePathAndSlug(t *testing.T) {
	tests := []struct {
		pt   PageType
		slug string
		path string
	}{
		{TypeEntity, "module-apps-ripple", "entities/module-apps-ripple.md"},
		{TypeSynthesis, "architecture-overview", "synthesis/architecture-overview.md"},
		{TypeOverview, "ignored", "overview.md"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := PagePath(tt.pt, tt.slug); got != tt.path {
				t.Errorf("PagePath = %q, want %q", got, tt.path)
			}
		})
	}

	if got := SlugFromPath("entities/module-apps-ripple.md"); got != "module-apps-ripple" {
		t.Errorf("SlugFromPath = %q", got)
	}
}

func TestValidSlug(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"module-apps-ripple", true},
		{"uspto", true},
		{"a1-b2", true},
		{"Module-Apps", false},
		{"module_apps", false},
		{"-leading", false},
		{"trailing-", false},
		{"double--dash", false},
		{"has space", false},
		{"", false},
		{"../escape", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ValidSlug(tt.in); got != tt.want {
				t.Errorf("ValidSlug(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsReserved(t *testing.T) {
	for _, p := range []string{"index.md", "overview.md", "log.md", "./index.md"} {
		if !IsReserved(p) {
			t.Errorf("IsReserved(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"entities/index.md", "concepts/log.md", "entities/x.md"} {
		if IsReserved(p) {
			t.Errorf("IsReserved(%q) = true, want false", p)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
