package wiki

import (
	"regexp"
	"sort"
	"strings"
)

// wikilinkPattern matches [[target]] and [[target|alias]]. Targets may be bare
// slugs ("kafka-consumer-groups") or directory-qualified
// ("concepts/kafka-consumer-groups"); both resolve to the same page.
var wikilinkPattern = regexp.MustCompile(`\[\[([^\]\|]+?)(?:\|([^\]]*?))?\]\]`)

// Link is one wikilink occurrence in a page body.
type Link struct {
	Raw    string // the full "[[...]]" text
	Target string // normalized slug
	Alias  string // display text, empty when absent
}

// ExtractLinks returns every wikilink in a body, in order of appearance and
// deduplicated by target.
func ExtractLinks(body string) []Link {
	matches := wikilinkPattern.FindAllStringSubmatch(body, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]Link, 0, len(matches))

	for _, m := range matches {
		target := NormalizeLinkTarget(m[1])
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, Link{Raw: m[0], Target: target, Alias: strings.TrimSpace(m[2])})
	}
	return out
}

// NormalizeLinkTarget reduces a link target to a bare slug. A directory prefix
// is dropped so "concepts/foo" and "foo" are the same page, and a trailing .md
// is tolerated because models write it often.
func NormalizeLinkTarget(target string) string {
	t := strings.TrimSpace(target)
	t = strings.TrimSuffix(t, ".md")
	if i := strings.LastIndex(t, "/"); i >= 0 {
		t = t[i+1:]
	}
	// Anchors point within a page, not at a different one.
	if i := strings.Index(t, "#"); i >= 0 {
		t = t[:i]
	}
	return strings.ToLower(strings.TrimSpace(t))
}

// LinkTargets returns just the normalized targets.
func LinkTargets(body string) []string {
	links := ExtractLinks(body)
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.Target
	}
	return out
}

// ResolveLinks partitions a body's links into those that resolve against known
// and returns the unresolved ones sorted for stable reporting.
//
// Unresolved links are not merely an error condition: they are the cheapest and
// most precise gap signal available, since the wiki has explicitly declared it
// wants a page that does not exist.
func ResolveLinks(body string, known map[string]bool) (resolved, unresolved []string) {
	for _, l := range ExtractLinks(body) {
		if known[l.Target] {
			resolved = append(resolved, l.Target)
		} else {
			unresolved = append(unresolved, l.Target)
		}
	}
	sort.Strings(resolved)
	sort.Strings(unresolved)
	return resolved, unresolved
}

// PruneDeadLinks rewrites unresolvable wikilinks to plain text, preserving the
// alias when one was given. Returns the new body and the targets removed.
//
// Rewriting rather than deleting keeps the sentence readable: a reader should
// still see what the page meant to reference.
func PruneDeadLinks(body string, known map[string]bool) (string, []string) {
	var removed []string
	seen := map[string]bool{}

	out := wikilinkPattern.ReplaceAllStringFunc(body, func(raw string) string {
		m := wikilinkPattern.FindStringSubmatch(raw)
		target := NormalizeLinkTarget(m[1])
		if known[target] {
			return raw
		}
		if !seen[target] {
			seen[target] = true
			removed = append(removed, target)
		}
		if alias := strings.TrimSpace(m[2]); alias != "" {
			return alias
		}
		return strings.TrimSpace(m[1])
	})

	sort.Strings(removed)
	return out, removed
}

// FilterRelated drops entries from a related list that no longer name a live
// page, which is what keeps frontmatter honest after a cascade deletion.
func FilterRelated(related []string, known map[string]bool) []string {
	if len(related) == 0 {
		return nil
	}
	out := make([]string, 0, len(related))
	for _, r := range related {
		if known[NormalizeLinkTarget(r)] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
