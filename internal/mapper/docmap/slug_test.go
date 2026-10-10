package docmap

import "testing"

// The expected values were recorded from the implementation before the slug
// helpers moved into package mapper: a slug is page identity, so a change in
// how text becomes one silently renames pages.
func TestSlugifyPinnedOutputs(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "document"},
		{" ", "document"},
		{".", "document"},
		{"!!!", "document"},
		{"Hello, World!", "hello-world"},
		{"a_b", "a-b"},
		{"a--b", "a-b"},
		{"-a-", "a"},
		{"A/B/C", "a-b-c"},
		{"README.md", "readme-md"},
		{"Ünïcödé", "n-c-d"},
		{"über/straße", "ber-stra-e"},
		{"naïve café", "na-ve-caf"},
		{"日本語", "document"},
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
		{"ΑΒΓ", "document"},
		{"\n\nline\n", "line"},
		{"c++/c#", "c-c"},
		{"a.b.c/d-e_f", "a-b-c-d-e-f"},
		{"ÀÉÎ", "document"},
	} {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFileSlugPinnedOutputs(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "document"},
		{" ", "document"},
		{".", "document"},
		{"!!!", "document"},
		{"Hello, World!", "hello-world"},
		{"a_b", "a-b"},
		{"a--b", "a-b"},
		{"-a-", "a"},
		{"A/B/C", "a-b-c"},
		{"README.md", "readme-md"},
		{"Ünïcödé", "n-c-d"},
		{"über/straße", "ber-stra-e"},
		{"naïve café", "na-ve-caf"},
		{"日本語", "document"},
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
		{"ΑΒΓ", "document"},
		{"\n\nline\n", "line"},
		{"c++/c#", "c-c"},
		{"a.b.c/d-e_f", "a-b-c-d-e-f"},
		{"ÀÉÎ", "document"},
		{"word-word-word-word-word-word-word-word-word-word-word-word-word-word-word-word-word-word-word-word-end", "word-word-word-word-word-word-word-word-word-word-word-word"},
	} {
		if got := fileSlug(tt.in); got != tt.want {
			t.Errorf("fileSlug(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
