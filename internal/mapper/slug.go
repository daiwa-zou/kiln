package mapper

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	nonSlug      = regexp.MustCompile(`[^a-z0-9]+`)
	dashCollapse = regexp.MustCompile(`-{2,}`)
)

// Slugify converts a path or name into a stable, filesystem- and URL-safe slug:
// lowercase ASCII letters and digits joined by single dashes. Directory
// separators become dashes so nested modules stay distinguishable:
// "apps/ripple" and "apps-ripple" would otherwise collide, which is why the
// caller is responsible for uniqueness (see UniqueSlug), not this function.
//
// fallback is returned when nothing slug-worthy survives -- an empty string,
// punctuation only, or a name written entirely outside ASCII -- and is the one
// thing the mappers legitimately differ on ("root" for a directory, "document"
// for a file).
//
// A slug is page identity, and Merge combines maps produced by different
// mappers, so every mapper must derive slugs here rather than keep its own copy
// that could drift.
func Slugify(s, fallback string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = dashCollapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return fallback
	}
	return s
}

// UniqueSlug returns base, or base-2, base-3, ... if already taken, and records
// the result in taken. Callers pass one shared map so slugs are unique across a
// whole map.
func UniqueSlug(taken map[string]bool, base string) string {
	if !taken[base] {
		taken[base] = true
		return base
	}
	for i := 2; ; i++ {
		candidate := base + "-" + strconv.Itoa(i)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}
