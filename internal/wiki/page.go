// Package wiki owns the structural half of a knowledge base: page frontmatter,
// the deterministic index/overview/log emitters, wikilink resolution, and
// validation of agent output.
//
// The LLM never writes the index, overview, or log. They are rebuilt from page
// frontmatter on every run, so navigation cannot drift from content.
package wiki

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// PageType is one of the six generated page types plus the singleton overview.
type PageType string

const (
	TypeEntity     PageType = "entity"
	TypeConcept    PageType = "concept"
	TypeSource     PageType = "source"
	TypeQuery      PageType = "query"
	TypeComparison PageType = "comparison"
	TypeSynthesis  PageType = "synthesis"
	TypeOverview   PageType = "overview"
)

// GeneratedTypes are the types an agent may produce. Overview is excluded: it
// is emitted deterministically and is never agent-writable.
var GeneratedTypes = []PageType{
	TypeEntity, TypeConcept, TypeSource, TypeQuery, TypeComparison, TypeSynthesis,
}

// typeDirs maps each page type to its directory.
//
// Spelled out rather than derived by appending "s": English pluralization would
// give entitys, querys, and synthesiss, and these directory names are part of
// the on-disk contract with Obsidian and the llm_wiki export.
var typeDirs = map[PageType]string{
	TypeEntity:     "entities",
	TypeConcept:    "concepts",
	TypeSource:     "sources",
	TypeQuery:      "queries",
	TypeComparison: "comparisons",
	TypeSynthesis:  "synthesis",
	TypeOverview:   "",
}

// Dir returns the directory a page type lives in. The directory and the
// frontmatter type must agree, which validation enforces.
func (t PageType) Dir() string { return typeDirs[t] }

// Valid reports whether t is a known page type.
func (t PageType) Valid() bool {
	switch t {
	case TypeEntity, TypeConcept, TypeSource, TypeQuery,
		TypeComparison, TypeSynthesis, TypeOverview:
		return true
	}
	return false
}

// Generated reports whether an agent is permitted to write this type.
func (t PageType) Generated() bool {
	for _, g := range GeneratedTypes {
		if t == g {
			return true
		}
	}
	return false
}

// TypeFromDir maps a directory name back to its page type.
func TypeFromDir(dir string) (PageType, bool) {
	for _, t := range GeneratedTypes {
		if t.Dir() == dir {
			return t, true
		}
	}
	return "", false
}

// Page is a single wiki page: frontmatter plus markdown body.
type Page struct {
	// Path is wiki-relative, e.g. "entities/module-apps-ripple.md".
	Path string
	Slug string
	Meta Frontmatter
	Body string
}

// ReservedPages are emitted deterministically and may never be written by an
// agent. Attempting to do so is a validation failure, not a warning.
var ReservedPages = []string{"index.md", "overview.md", "log.md"}

// IsReserved reports whether a wiki-relative path is deterministically owned.
func IsReserved(path string) bool {
	clean := strings.TrimPrefix(strings.TrimSpace(path), "./")
	for _, r := range ReservedPages {
		if clean == r {
			return true
		}
	}
	return false
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidSlug reports whether s is an acceptable page slug. Slugs are constrained
// so a page path can never escape the wiki directory or collide case-wise on a
// case-insensitive filesystem.
func ValidSlug(s string) bool {
	return slugPattern.MatchString(s)
}

// PagePath builds the wiki-relative path for a type and slug.
func PagePath(t PageType, slug string) string {
	if t == TypeOverview {
		return "overview.md"
	}
	return fmt.Sprintf("%s/%s.md", t.Dir(), slug)
}

// SlugFromPath extracts the slug from a wiki-relative page path.
func SlugFromPath(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".md")
}

// ContentHash is a stable digest of a page's rendered form, used to detect
// whether a regeneration actually changed anything.
func (p Page) ContentHash() string {
	sum := sha256.Sum256([]byte(p.Render()))
	return hex.EncodeToString(sum[:])
}

// Render serializes the page back to frontmatter plus body.
func (p Page) Render() string {
	var b strings.Builder
	b.WriteString(p.Meta.Render())
	b.WriteString("\n")
	b.WriteString(strings.TrimLeft(p.Body, "\n"))
	if !strings.HasSuffix(p.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}
