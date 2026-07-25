package wiki

import (
	"fmt"
	"path"
	"strings"
)

// MinBodyBytes rejects stub pages. An agent that runs low on budget tends to
// emit a heading and nothing else, which is worse than no page at all because
// it looks like coverage.
const MinBodyBytes = 120

// Violation is a single validation failure, carrying enough detail to be fed
// back to the agent as a corrective instruction.
type Violation struct {
	Path   string
	Reason string
}

func (v Violation) String() string {
	if v.Path == "" {
		return v.Reason
	}
	return fmt.Sprintf("%s: %s", v.Path, v.Reason)
}

// ValidateOptions carries the context a page is checked against.
type ValidateOptions struct {
	// Planned is the set of wiki-relative paths the approved plan authorized.
	// Anything outside it was not agreed to and is quarantined.
	Planned map[string]bool
	// KnownSlugs are pages that already exist or are being written in this same
	// batch, used to resolve wikilinks.
	KnownSlugs map[string]bool
	// MaxUnresolvedLinks tolerated before the page is rejected outright. A
	// couple of dangling links are pruned and reported; many mean the agent
	// invented a structure that does not exist.
	MaxUnresolvedLinks int
	// RequireDates enforces created/updated. Disabled when the caller stamps
	// them itself after validation.
	RequireDates bool
}

// DefaultMaxUnresolvedLinks is the tolerance before a page fails.
const DefaultMaxUnresolvedLinks = 3

// ValidatePage checks one agent-written page. It returns every violation rather
// than the first, so a single corrective turn can address all of them.
func ValidatePage(p *Page, opts ValidateOptions) []Violation {
	var out []Violation
	add := func(format string, args ...any) {
		out = append(out, Violation{Path: p.Path, Reason: fmt.Sprintf(format, args...)})
	}

	if err := checkPath(p.Path); err != nil {
		add("%s", err.Error())
		// Every later check assumes a sane path, so stop here.
		return out
	}

	if IsReserved(p.Path) {
		add("%s is maintained by kiln and must never be written by an agent", p.Path)
		return out
	}

	if len(opts.Planned) > 0 && !opts.Planned[p.Path] {
		add("not in the approved plan")
	}

	if !p.Meta.Type.Valid() {
		add("unknown page type %q", p.Meta.Type)
	} else if !p.Meta.Type.Generated() {
		add("page type %q is not agent-writable", p.Meta.Type)
	} else if dir := path.Dir(p.Path); dir != p.Meta.Type.Dir() {
		// A concept page filed under entities/ would break every path
		// assumption downstream, including the index.
		add("type %q does not match directory %q", p.Meta.Type, dir)
	}

	if strings.TrimSpace(p.Meta.Title) == "" {
		add("title is empty")
	}
	if !ValidSlug(p.Slug) {
		add("slug %q must be lowercase kebab-case", p.Slug)
	}

	if opts.RequireDates {
		if !ValidDate(p.Meta.Created) {
			add("created %q is not %s", p.Meta.Created, DateFormat)
		}
		if !ValidDate(p.Meta.Updated) {
			add("updated %q is not %s", p.Meta.Updated, DateFormat)
		}
	}

	body := strings.TrimSpace(p.Body)
	switch {
	case body == "":
		add("body is empty")
	case len(body) < MinBodyBytes:
		add("body is %d bytes, below the %d-byte minimum", len(body), MinBodyBytes)
	}
	if !hasHeading(p.Body) {
		add("body has no markdown heading")
	}
	if strings.Contains(p.Body, "---FILE:") || strings.Contains(p.Body, "---END FILE---") {
		add("body contains leftover block-protocol markers")
	}

	limit := opts.MaxUnresolvedLinks
	if limit == 0 {
		limit = DefaultMaxUnresolvedLinks
	}
	if opts.KnownSlugs != nil {
		if _, unresolved := ResolveLinks(p.Body, opts.KnownSlugs); len(unresolved) > limit {
			add("%d unresolved wikilinks exceed the limit of %d: %s",
				len(unresolved), limit, strings.Join(unresolved, ", "))
		}
	}

	return out
}

// checkPath rejects anything that could escape the wiki directory or land
// outside a page directory. This runs before any file is read.
func checkPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute paths are not allowed")
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("backslashes are not allowed in wiki paths")
	}
	clean := path.Clean(p)
	if clean != p {
		return fmt.Errorf("path is not in canonical form (want %q)", clean)
	}
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("path escapes the wiki directory")
	}
	if !strings.HasSuffix(clean, ".md") {
		return fmt.Errorf("page files must end in .md")
	}

	// Reserved files sit at the wiki root; every other page must be in a
	// recognized type directory.
	if !strings.Contains(clean, "/") {
		if IsReserved(clean) {
			return nil
		}
		return fmt.Errorf("pages must live in a type directory")
	}

	parts := strings.Split(clean, "/")
	if len(parts) != 2 {
		return fmt.Errorf("pages must be exactly one level deep")
	}
	if _, ok := TypeFromDir(parts[0]); !ok {
		return fmt.Errorf("unknown page directory %q", parts[0])
	}
	return nil
}

func hasHeading(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			return true
		}
	}
	return false
}

// ValidateBatch checks a whole set of pages, resolving links against pages
// written in the same run as well as those already present.
func ValidateBatch(pages []*Page, opts ValidateOptions) []Violation {
	known := make(map[string]bool, len(opts.KnownSlugs)+len(pages))
	for slug := range opts.KnownSlugs {
		known[slug] = true
	}
	for _, p := range pages {
		known[p.Slug] = true
	}
	opts.KnownSlugs = known

	seen := make(map[string]bool, len(pages))
	var out []Violation

	for _, p := range pages {
		if seen[p.Path] {
			out = append(out, Violation{Path: p.Path, Reason: "written more than once in one run"})
			continue
		}
		seen[p.Path] = true
		out = append(out, ValidatePage(p, opts)...)
	}
	return out
}
