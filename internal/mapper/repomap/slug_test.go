package repomap

import "testing"

// The expected values were recorded from the implementation before the slug
// helpers moved into package mapper. A slug is page identity and mapper.Merge
// combines maps built by different mappers, so a change in how text becomes a
// slug silently renames pages.
func TestSlugifyPinnedOutputs(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "root"},
		{" ", "root"},
		{".", "root"},
		{"!!!", "root"},
		{"Hello, World!", "hello-world"},
		{"a_b", "a-b"},
		{"a--b", "a-b"},
		{"-a-", "a"},
		{"A/B/C", "a-b-c"},
		{"README.md", "readme-md"},
		{"Ünïcödé", "n-c-d"},
		{"über/straße", "ber-stra-e"},
		{"naïve café", "na-ve-caf"},
		{"日本語", "root"},
		{"日本語 report", "report"},
		{"emoji 😀 test", "emoji-test"},
		{"tab\tsep", "tab-sep"},
		{"\u00a0nbsp\u00a0", "nbsp"},
		{"İstanbul", "istanbul"},
		{"x\xffy", "x-y"},
		{"123", "123"},
		{"v1.2.3", "v1-2-3"},
		{"a   b", "a-b"},
		{"Q3 Report (final)", "q3-report-final"},
		{"__init__.py", "init-py"},
		{"ΑΒΓ", "root"},
		{"\n\nline\n", "line"},
		{"c++/c#", "c-c"},
		{"a.b.c/d-e_f", "a-b-c-d-e-f"},
		{"ÀÉÎ", "root"},
	} {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Collision suffixes must keep counting past a suffix that is itself taken.
