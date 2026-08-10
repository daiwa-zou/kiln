package naming

import (
	"strings"
	"testing"
)

func TestFromFilenameReadsLikeALabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The shapes an upload actually arrives in.
		{"NBAE5150_syllabus_v3_FINAL.pdf", "NBAE5150 Syllabus"},
		{"quarterly-report-2026-03-14.pdf", "Quarterly Report"},
		{"Scan_2026-03-14_11-42-08.pdf", "Scan"},
		{"meeting notes (1).docx", "Meeting Notes"},
		{"doc-8f3a9c2b.docx", "Doc"},
		{"budget_2026.xlsx", "Budget"},

		// Minor words stay down in the middle and up at the edges.
		{"the_state_of_the_union.md", "The State of the Union"},
		{"notes_on_hiring.md", "Notes on Hiring"},

		// Anything its author capitalized deliberately is left alone.
		{"McClean_interview.txt", "McClean Interview"},
		{"iPhone_teardown.md", "iPhone Teardown"},

		// A lone number is usually load-bearing; a run of them is a timestamp.
		{"form_1099_instructions.pdf", "Form 1099 Instructions"},
		{"route_66_history.md", "Route 66 History"},

		// A name of pure noise keeps the stem: a blank label is worse.
		{"FINAL_copy_v2.pdf", "FINAL_copy_v2"},

		// Paths and backslashes reduce to the base name.
		{`C:\Users\dz\Desktop\team handbook.md`, "Team Handbook"},
		{"nested/dir/hiring plan.md", "Hiring Plan"},

		// Never empty, whatever arrives. A file that is nothing but an
		// extension has only that to be called.
		{".pdf", "Pdf"},
		{"", "."},
	} {
		if got := FromFilename(tc.in); got != tc.want {
			t.Errorf("FromFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The derived name is a label, not an identifier: two different uploads may
// well produce the same one, and nothing downstream may assume otherwise.
// Uniqueness stays with the path, which is what the row is keyed on.
func TestFromFilenameIsNotAnIdentifier(t *testing.T) {
	a := FromFilename("budget_v1.xlsx")
	b := FromFilename("budget_v2.xlsx")
	if a != b {
		t.Fatalf("expected version tokens to collapse: %q vs %q", a, b)
	}
}

// The ingest-time answer. A filename can only ever be a guess at what a
// document is; once the text is in hand, the document usually just says.
func TestFromDocumentPrefersWhatTheDocumentCallsItself(t *testing.T) {
	for _, tc := range []struct{ name, file, text, want string }{
		{
			name: "a scan named by its scanner is named by its contents",
			file: "Scan_2026-03-14_11-42-08.pdf",
			text: "# Leadership Insights Syllabus (NBAE 5150, Fall 2026)\n\nWeek 1...",
			want: "Leadership Insights Syllabus (NBAE 5150, Fall 2026)",
		},
		{
			name: "no heading falls back to the filename, same convention",
			file: "NBAE5150_syllabus_v3_FINAL.pdf",
			text: "Week 1: introductions\nWeek 2: meritocracy",
			want: "NBAE5150 Syllabus",
		},
		{
			name: "the heading goes through the same noise filter",
			file: "memo.docx",
			text: "# Q3 Board Memo v2 FINAL\n\nBody",
			want: "Q3 Board Memo",
		},
		{
			name: "a heading that is only noise falls back rather than emptying",
			file: "quarterly-report-2026-03-14.pdf",
			text: "# FINAL\n\nBody",
			want: "Quarterly Report",
		},
		{
			name: "author capitalisation in a heading is preserved",
			file: "interview.txt",
			text: "# McClean on iPhone Adoption\n",
			want: "McClean on iPhone Adoption",
		},
		{
			name: "a section heading is not the document's title",
			file: "handbook.md",
			text: "## Chapter One\n\nBody",
			want: "Handbook",
		},
		{
			name: "empty text is just the filename case",
			file: "budget_2026.xlsx",
			text: "",
			want: "Budget",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromDocument(tc.file, tc.text); got != tc.want {
				t.Errorf("FromDocument(%q, ...) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

// A heading buried past the opening is a chapter, not a title, and a whole
// extraction is too much text to scan for one anyway.
func TestFromDocumentIgnoresAHeadingFarIntoTheText(t *testing.T) {
	text := strings.Repeat("body text\n", 900) + "\n# Appendix B\n"
	if got := FromDocument("field-notes.pdf", text); got != "Field Notes" {
		t.Errorf("got %q, want the filename-derived name", got)
	}
}

// A document that opens with a sentence gets a name, not a paragraph.
func TestFromDocumentCapsARunawayHeading(t *testing.T) {
	long := "# " + strings.Repeat("Considerations Regarding The Matter At Hand ", 8)
	got := FromDocument("memo.pdf", long)
	if r := []rune(got); len(r) > maxNameRunes+1 {
		t.Fatalf("name is %d runes, want <= %d: %q", len(r), maxNameRunes+1, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a clipped name should say so: %q", got)
	}
}
