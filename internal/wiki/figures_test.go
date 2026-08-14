package wiki

import (
	"strings"
	"testing"
)

func TestFigureRefs(t *testing.T) {
	body := "# Report\n\n" +
		"Revenue grew.\n\n" +
		"![Revenue by quarter](figure:abc-123)\n\n" +
		"And again, same picture: ![again](figure:abc-123)\n\n" +
		"![Latency](figure:def-456)\n\n" +
		"An ordinary link is not a figure: [see this](https://example.com)\n" +
		"Nor is an ordinary image: ![logo](https://example.com/logo.png)\n"

	got := FigureRefs(body)
	if len(got) != 2 || got[0] != "abc-123" || got[1] != "def-456" {
		t.Errorf("FigureRefs = %v, want the two distinct figure ids in order", got)
	}
}

func TestValidateRejectsUnknownFigures(t *testing.T) {
	p := &Page{
		Path: "sources/report.md", Slug: "report",
		Meta: Frontmatter{Type: "source", Title: "Report", Created: "2026-08-13", Updated: "2026-08-13"},
		Body: "# Report\n\n" + strings.Repeat("Prose about the quarter. ", 8) +
			"\n\n![Revenue](figure:known-1)\n\n![Invented](figure:hallucinated-9)\n",
	}

	got := ValidatePage(p, ValidateOptions{
		AllowedFigures: map[string]bool{"known-1": true},
	})

	var found string
	for _, v := range got {
		if strings.Contains(v.Reason, "figure") {
			found = v.Reason
		}
	}
	if found == "" {
		t.Fatalf("no figure violation raised: %+v", got)
	}
	if !strings.Contains(found, "hallucinated-9") {
		t.Errorf("violation does not name the bad id: %q", found)
	}
	if strings.Contains(found, "known-1") {
		t.Errorf("violation wrongly blames the valid id: %q", found)
	}
}

func TestValidateAcceptsKnownFigures(t *testing.T) {
	p := &Page{
		Path: "sources/report.md", Slug: "report",
		Meta: Frontmatter{Type: "source", Title: "Report", Created: "2026-08-13", Updated: "2026-08-13"},
		Body: "# Report\n\n" + strings.Repeat("Prose about the quarter. ", 8) +
			"\n\n![Revenue](figure:known-1)\n",
	}

	for _, v := range ValidatePage(p, ValidateOptions{
		AllowedFigures: map[string]bool{"known-1": true, "known-2": true},
	}) {
		if strings.Contains(v.Reason, "figure") {
			t.Errorf("valid figure rejected: %q", v.Reason)
		}
	}
}

// TestValidateWithNoFigureTrackingIgnoresRefs: a nil map means the caller is
// not tracking figures, and the check must not fire. A repository-only build
// never loads figures and its pages must still validate.
func TestValidateWithNoFigureTrackingIgnoresRefs(t *testing.T) {
	p := &Page{
		Path: "sources/report.md", Slug: "report",
		Meta: Frontmatter{Type: "source", Title: "Report", Created: "2026-08-13", Updated: "2026-08-13"},
		Body: "# Report\n\n" + strings.Repeat("Prose about the quarter. ", 8) +
			"\n\n![Revenue](figure:whatever)\n",
	}

	for _, v := range ValidatePage(p, ValidateOptions{}) {
		if strings.Contains(v.Reason, "figure") {
			t.Errorf("figure check fired with no allowed set: %q", v.Reason)
		}
	}
}

// TestValidateRejectsFiguresWhenUnitHasNone: an empty non-nil map is the unit
// saying it has no pictures, so any reference is invented.
func TestValidateRejectsFiguresWhenUnitHasNone(t *testing.T) {
	p := &Page{
		Path: "sources/report.md", Slug: "report",
		Meta: Frontmatter{Type: "source", Title: "Report", Created: "2026-08-13", Updated: "2026-08-13"},
		Body: "# Report\n\n" + strings.Repeat("Prose about the quarter. ", 8) +
			"\n\n![Revenue](figure:made-up)\n",
	}

	var fired bool
	for _, v := range ValidatePage(p, ValidateOptions{AllowedFigures: map[string]bool{}}) {
		if strings.Contains(v.Reason, "figure") {
			fired = true
		}
	}
	if !fired {
		t.Error("a unit with no figures accepted a figure reference")
	}
}

func TestResolveFigures(t *testing.T) {
	body := "![Revenue](figure:abc)\n\n![Other](figure:def)\n\n![external](https://example.com/x.png)\n"

	got := ResolveFigures(body, func(id string) string {
		if id == "abc" {
			return "/api/v1/workspaces/demo/figures/abc"
		}
		// An id that resolves to nothing is left exactly as it was, rather
		// than rewritten to a broken URL.
		return ""
	})

	if !strings.Contains(got, "![Revenue](/api/v1/workspaces/demo/figures/abc)") {
		t.Errorf("known figure not resolved:\n%s", got)
	}
	if !strings.Contains(got, "![Other](figure:def)") {
		t.Errorf("unresolvable figure was rewritten:\n%s", got)
	}
	if !strings.Contains(got, "![external](https://example.com/x.png)") {
		t.Errorf("ordinary image was touched:\n%s", got)
	}
}
