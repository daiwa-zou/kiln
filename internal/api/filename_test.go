package api

import "testing"

func TestDescriptiveNameReadsLikeALabel(t *testing.T) {
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
		if got := descriptiveName(tc.in); got != tc.want {
			t.Errorf("descriptiveName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The derived name is a label, not an identifier: two different uploads may
// well produce the same one, and nothing downstream may assume otherwise.
// Uniqueness stays with the path, which is what the row is keyed on.
func TestDescriptiveNameIsNotAnIdentifier(t *testing.T) {
	a := descriptiveName("budget_v1.xlsx")
	b := descriptiveName("budget_v2.xlsx")
	if a != b {
		t.Fatalf("expected version tokens to collapse: %q vs %q", a, b)
	}
}
