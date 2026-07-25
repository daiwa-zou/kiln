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

func TestPruneDeadLinks(t *testing.T) {
	body := "See [[alpha]] and [[gone]] and [[also-gone|the other thing]].\n"
	known := map[string]bool{"alpha": true}

	got, removed := PruneDeadLinks(body, known)

	// A live link survives; a dead one becomes plain text, keeping its alias so
	// the sentence still reads correctly.
	want := "See [[alpha]] and gone and the other thing.\n"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if wantRemoved := []string{"also-gone", "gone"}; !reflect.DeepEqual(removed, wantRemoved) {
		t.Errorf("removed = %v, want %v", removed, wantRemoved)
	}
}

func TestPruneDeadLinksNoChange(t *testing.T) {
	body := "All good: [[alpha]] and [[beta]].\n"
	known := map[string]bool{"alpha": true, "beta": true}

	got, removed := PruneDeadLinks(body, known)
	if got != body {
		t.Errorf("body was rewritten unnecessarily:\n got %q\nwant %q", got, body)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
}

func TestFilterRelated(t *testing.T) {
	known := map[string]bool{"alpha": true, "beta": true}

	tests := []struct {
		name    string
		related []string
		want    []string
	}{
		{"all live", []string{"alpha", "beta"}, []string{"alpha", "beta"}},
		{"drops dead entries", []string{"alpha", "gone", "beta"}, []string{"alpha", "beta"}},
		{"normalizes before matching", []string{"concepts/alpha"}, []string{"concepts/alpha"}},
		{"all dead yields nil", []string{"gone", "also-gone"}, nil},
		{"empty yields nil", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterRelated(tt.related, known)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FilterRelated(%v) = %v, want %v", tt.related, got, tt.want)
			}
		})
	}
}

func TestExtractLinksIgnoresNonLinks(t *testing.T) {
	// Single brackets and unterminated pairs are ordinary markdown, not links.
	body := "A [reference][1] and [a link](http://x) and [[unterminated\n"
	if got := ExtractLinks(body); len(got) != 0 {
		t.Errorf("ExtractLinks = %+v, want none", got)
	}
}
