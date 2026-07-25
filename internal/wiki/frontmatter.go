package wiki

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DateFormat is the frontmatter date layout. Dates are days, not timestamps:
// a page regenerated twice in one day should not look like it changed.
const DateFormat = "2006-01-02"

// Frontmatter is the YAML header every page carries.
type Frontmatter struct {
	Type    PageType `yaml:"type"`
	Title   string   `yaml:"title"`
	Created string   `yaml:"created"`
	Updated string   `yaml:"updated"`
	Tags    []string `yaml:"tags,omitempty"`
	Related []string `yaml:"related,omitempty"`
	Sources []string `yaml:"sources,omitempty"`
	// Provenance distinguishes claims grounded in the user's own sources from
	// those derived from web research, which is lower-trust.
	Provenance string `yaml:"provenance,omitempty"`
	// BuiltAtRef records the source revision this page reflects, so a reader
	// (or an agent) can tell how stale it is.
	BuiltAtRef string `yaml:"built_at_ref,omitempty"`
}

// ErrNoFrontmatter is returned when a document has no YAML header at all.
var ErrNoFrontmatter = errors.New("wiki: missing frontmatter")

const fence = "---"

// ParsePage splits a raw markdown document into frontmatter and body.
func ParsePage(path string, raw []byte) (*Page, error) {
	meta, body, err := ParseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("wiki: %s: %w", path, err)
	}
	return &Page{
		Path: path,
		Slug: SlugFromPath(path),
		Meta: *meta,
		Body: body,
	}, nil
}

// ParseFrontmatter extracts the YAML header and returns it with the remaining
// body. A document without a leading fence is an error rather than a page with
// empty metadata, because silently accepting one would let an unvalidated page
// into the index.
func ParseFrontmatter(raw []byte) (*Frontmatter, string, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	// Strip leading blank lines and a UTF-8 BOM, both of which appear in
	// agent-written files often enough to be worth tolerating.
	trimmed := strings.TrimLeft(text, "\n\ufeff")

	if !strings.HasPrefix(trimmed, fence+"\n") {
		return nil, "", ErrNoFrontmatter
	}

	rest := trimmed[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return nil, "", fmt.Errorf("wiki: unterminated frontmatter")
	}

	header := rest[:end]
	// end points at the newline before the closing fence; skip past the fence
	// itself, then drop the newline that terminates it plus any blank lines, so
	// the body starts at its first real content.
	body := strings.TrimLeft(rest[end+len(fence)+1:], "\n")

	var meta Frontmatter
	if err := yaml.Unmarshal([]byte(header), &meta); err != nil {
		return nil, "", fmt.Errorf("wiki: parse frontmatter: %w", err)
	}
	return &meta, body, nil
}

// Render serializes frontmatter deterministically.
//
// Field order is fixed rather than delegated to a YAML marshaller so that
// rewriting an unchanged page produces byte-identical output; otherwise every
// run would look like it modified every page.
func (f Frontmatter) Render() string {
	var b strings.Builder
	b.WriteString(fence + "\n")
	fmt.Fprintf(&b, "type: %s\n", f.Type)
	fmt.Fprintf(&b, "title: %s\n", yamlScalar(f.Title))
	if f.Created != "" {
		fmt.Fprintf(&b, "created: %s\n", f.Created)
	}
	if f.Updated != "" {
		fmt.Fprintf(&b, "updated: %s\n", f.Updated)
	}
	if len(f.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", yamlFlowSeq(f.Tags))
	}
	if len(f.Related) > 0 {
		fmt.Fprintf(&b, "related: %s\n", yamlFlowSeq(f.Related))
	}
	if len(f.Sources) > 0 {
		fmt.Fprintf(&b, "sources: %s\n", yamlQuotedSeq(f.Sources))
	}
	if f.Provenance != "" {
		fmt.Fprintf(&b, "provenance: %s\n", f.Provenance)
	}
	if f.BuiltAtRef != "" {
		fmt.Fprintf(&b, "built_at_ref: %s\n", f.BuiltAtRef)
	}
	b.WriteString(fence + "\n")
	return b.String()
}

// Today returns the current date in frontmatter format.
func Today() string { return time.Now().UTC().Format(DateFormat) }

// ValidDate reports whether s is a well-formed frontmatter date.
func ValidDate(s string) bool {
	_, err := time.Parse(DateFormat, s)
	return err == nil
}

// yamlScalar quotes a value only when it would otherwise be misparsed, keeping
// the common case readable in an editor.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := strings.ContainsAny(s, `:#{}[]&*!|>'"%@`+"`") ||
		strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") ||
		looksNonString(s)
	if !needsQuote {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// looksNonString catches titles a YAML parser would read as a bool, null, or
// number rather than text.
func looksNonString(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
		return true
	}
	return false
}

func yamlFlowSeq(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = yamlScalar(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// yamlQuotedSeq always quotes, used for sources where entries are filenames
// that frequently contain characters YAML would otherwise interpret.
func yamlQuotedSeq(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
