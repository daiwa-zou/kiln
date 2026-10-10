package agent

import "testing"

// The fake runner's slugs are page identity within a fake build, so their exact
// output is pinned across moves of the helper that computes them.
func TestFakeSlugifyPinnedOutputs(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "unit"},
		{" ", "unit"},
		{".", "unit"},
		{"!!!", "unit"},
		{"Hello, World!", "hello-world"},
		{"a_b", "a-b"},
		{"a--b", "a-b"},
		{"-a-", "a"},
		{"A/B/C", "a-b-c"},
		{"README.md", "readme-md"},
		{"Ünïcödé", "n-c-d"},
		{"über/straße", "ber-stra-e"},
		{"naïve café", "na-ve-caf"},
		{"日本語", "unit"},
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
		{"ΑΒΓ", "unit"},
		{"\n\nline\n", "line"},
		{"c++/c#", "c-c"},
		{"a.b.c/d-e_f", "a-b-c-d-e-f"},
		{"ÀÉÎ", "unit"},
	} {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
