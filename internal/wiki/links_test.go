package wiki

import (
	"reflect"
	"testing"
)

func TestExtractLinks(t *testing.T) {
	body := `# Task Dispatch

Dispatch is coordinated by [[ripple]] and observed by [[beacon|the beacon service]].
It builds on [[concepts/backpressure]] and mentions [[ripple]] again.
See also [[queries/who-owns-retries.md]].
`

	got := ExtractLinks(body)
	want := []Link{
		{Raw: "[[ripple]]", Target: "ripple"},
		{Raw: "[[beacon|the beacon service]]", Target: "beacon", Alias: "the beacon service"},
		{Raw: "[[concepts/backpressure]]", Target: "backpressure"},
		{Raw: "[[queries/who-owns-retries.md]]", Target: "who-owns-retries"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractLinks =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestNormalizeLinkTarget(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ripple", "ripple"},
		{"concepts/backpressure", "backpressure"},
		{"entities/module-apps-ripple.md", "module-apps-ripple"},
		{"  spaced  ", "spaced"},
		{"Backpressure", "backpressure"},
		{"backpressure#section", "backpressure"},
		{"a/b/c/deep", "deep"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := NormalizeLinkTarget(tt.in); got != tt.want {
				t.Errorf("NormalizeLinkTarget(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveLinks(t *testing.T) {
	body := "Links to [[alpha]], [[beta]], and [[missing]].\n"
	known := map[string]bool{"alpha": true, "beta": true}

	resolved, unresolved := ResolveLinks(body, known)

	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(resolved, want) {
		t.Errorf("resolved = %v, want %v", resolved, want)
	}
	if want := []string{"missing"}; !reflect.DeepEqual(unresolved, want) {
		t.Errorf("unresolved = %v, want %v", unresolved, want)
	}
}

func TestExtractLinksIgnoresNonLinks(t *testing.T) {
	// Single brackets and unterminated pairs are ordinary markdown, not links.
	body := "A [reference][1] and [a link](http://x) and [[unterminated\n"
	if got := ExtractLinks(body); len(got) != 0 {
		t.Errorf("ExtractLinks = %+v, want none", got)
	}
}
