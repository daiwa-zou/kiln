package repomap

import (
	"regexp"
	"strings"
)

var (
	nonSlug      = regexp.MustCompile(`[^a-z0-9]+`)
	dashCollapse = regexp.MustCompile(`-{2,}`)
)

// Slugify converts a path or name into a stable, filesystem- and URL-safe slug.
// Directory separators become dashes so nested modules stay distinguishable:
// "apps/ripple" and "apps-ripple" would otherwise collide, which is why the
// caller is responsible for uniqueness, not this function.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = dashCollapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "root"
	}
	return s
}

// uniqueSlug returns base, or base-2, base-3, ... if already taken. Callers
// pass a shared map so slugs are unique across a whole map.
func uniqueSlug(taken map[string]bool, base string) string {
	if !taken[base] {
		taken[base] = true
		return base
	}
	for i := 2; ; i++ {
		candidate := base + "-" + itoa(i)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
