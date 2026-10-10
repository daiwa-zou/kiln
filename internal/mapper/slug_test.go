package mapper

import (
	"strings"
	"testing"
)

func TestSlugifyUsesTheCallersFallback(t *testing.T) {
	// The fallback is the one thing mappers may differ on; everything else is
	// pinned per mapper in repomap and docmap against recorded outputs.
	for _, in := range []string{"", " ", ".", "!!!", "日本語"} {
		if got := Slugify(in, "root"); got != "root" {
			t.Errorf("Slugify(%q, root) = %q, want root", in, got)
		}
		if got := Slugify(in, "document"); got != "document" {
			t.Errorf("Slugify(%q, document) = %q, want document", in, got)
		}
	}
	if got := Slugify("Q3 Report (final)", "x"); got != "q3-report-final" {
		t.Errorf("Slugify = %q, want q3-report-final", got)
	}
}

func TestSlugifyIsIdempotent(t *testing.T) {
	for _, in := range []string{"apps/ripple", "InfraFlux", "github.com/x/y", "  a  b  ", "a_b--c"} {
		once := Slugify(in, "root")
		if twice := Slugify(once, "root"); twice != once {
			t.Errorf("Slugify not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestUniqueSlug(t *testing.T) {
	taken := map[string]bool{}
	var got []string
	for _, base := range []string{"shared", "shared", "shared", "other"} {
		got = append(got, UniqueSlug(taken, base))
	}
	if want := "shared,shared-2,shared-3,other"; strings.Join(got, ",") != want {
		t.Errorf("UniqueSlug sequence = %v, want %s", got, want)
	}
}

// A suffix that is itself already taken is skipped rather than reused, and a
// suffixed base keeps counting from its own name.
func TestUniqueSlugSkipsTakenSuffixes(t *testing.T) {
	taken := map[string]bool{"x": true, "x-2": true, "y-2": true}
	var got []string
	for _, base := range []string{"x", "x", "y", "y", "y-2", "x-2"} {
		got = append(got, UniqueSlug(taken, base))
	}
	if want := "x-3,x-4,y,y-3,y-2-2,x-2-2"; strings.Join(got, ",") != want {
		t.Errorf("UniqueSlug sequence = %v, want %s", got, want)
	}
}
