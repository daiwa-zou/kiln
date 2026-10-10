// Package docmap partitions documents into units.
//
// Where repomap partitions by manifest boundaries, docmap partitions by
// document and by heading structure, which is about as different as two
// mappers get while producing the same WorkspaceMap.
package docmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/extract"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// SplitMinBytes is the size above which a document is split by its top-level
// headings.
//
// A 300-page PDF is one expensive unit that produces one shapeless page; split
// by section it becomes several units that regenerate independently, so editing
// one chapter does not re-pay for the whole book.
const SplitMinBytes = 24_000

// Doc is one extracted document ready to be mapped.
type Doc struct {
	// Key is the stable cache key, usually "doc:<relative path>".
	Key string
	// Path is the document's location, used for the slug and for evidence links.
	Path string
	// Title is human-readable; falls back to the filename.
	Title string
	// Text is the extracted content.
	Text string
	// Hash is the extraction digest. Content-only, so restaging a document
	// never looks like a change.
	Hash string
	// Origin deep-links back to the source.
	Origin string
	// Figures are the pictures and graphs the document carried. Passengers
	// here: the mapper never reads them, but they were recovered by the same
	// extraction that produced Text and this is what carries them to the
	// pipeline that stores them.
	Figures []extract.Figure
}

// Mapper partitions documents into units.
type Mapper struct {
	// SplitMinBytes overrides the default split threshold.
	SplitMinBytes int
	// StageSections writes each section's text to the file its unit's Inputs
	// name, under the root passed to MapDocs. A pipeline that renders prompts
	// from those inputs sets it; a caller that only wants the partitioning
	// (the shape, the keys, the hashes) leaves it off and touches no disk.
	StageSections bool
	// Now is injectable so tests can pin GeneratedAt.
	Now func() time.Time
}

// sectionsDir is the staging subdirectory section texts live under. The dot
// prefix and the name keep it from colliding with a document staged at the
// same root, which is named for the user's own file.
const sectionsDir = ".kiln-sections"

// Kind names the source type stamped into the maps this mapper produces.
func (m *Mapper) Kind() string { return "doc" }

// MapDocs partitions documents into units.
func (m *Mapper) MapDocs(_ context.Context, root string, docs []Doc) (*mapper.WorkspaceMap, error) {
	wm := &mapper.WorkspaceMap{
		SchemaVersion: 1,
		Kind:          m.Kind(),
		Root:          root,
		GeneratedAt:   m.now(),
	}

	taken := map[string]bool{}
	sorted := make([]Doc, len(docs))
	copy(sorted, docs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	for _, doc := range sorted {
		units, err := m.unitsFor(root, doc, taken)
		if err != nil {
			return nil, err
		}
		// Inputs point at staged text, which is not under the run's source
		// directory. Recording the root here is what lets a merged map resolve
		// them; a doc unit that loses it reads no source at all.
		for i := range units {
			if units[i].Meta == nil {
				units[i].Meta = map[string]any{}
			}
			units[i].Meta[mapper.MetaRoot] = root
		}
		wm.Units = append(wm.Units, units...)

		// A split document's sections are parts of one whole, so the reader
		// gets edges tying them back to the parent rather than a flat list.
		if len(units) > 1 {
			for _, u := range units[1:] {
				wm.Edges = append(wm.Edges, mapper.Edge{
					From: u.Key, To: units[0].Key, Kind: "references",
				})
			}
		}
	}

	wm.Summary = m.render(wm)
	wm.Hash = hashUnits(wm.Units)
	return wm, nil
}

func (m *Mapper) unitsFor(root string, doc Doc, taken map[string]bool) ([]mapper.Unit, error) {
	title := doc.Title
	if title == "" {
		title = strings.TrimSuffix(path.Base(doc.Path), path.Ext(doc.Path))
	}

	whole := mapper.Unit{
		Key:    doc.Key,
		Kind:   "doc",
		Slug:   uniqueSlug(taken, slugify(strings.TrimSuffix(doc.Path, path.Ext(doc.Path)))),
		Title:  title,
		Inputs: []string{doc.Path},
		Hash:   doc.Hash,
		Meta:   map[string]any{"origin": doc.Origin, "bytes": len(doc.Text)},
	}

	sections := m.split(doc)
	if len(sections) == 0 {
		return []mapper.Unit{whole}, nil
	}

	units := []mapper.Unit{whole}
	for i, s := range sections {
		input, err := m.sectionInput(root, doc, i, s)
		if err != nil {
			return nil, err
		}
		units = append(units, mapper.Unit{
			Key:   doc.Key + "#" + slugify(s.Title),
			Kind:  "doc-section",
			Slug:  uniqueSlug(taken, slugify(title+"-"+s.Title)),
			Title: s.Title,
			// The section's own text, not its parent document. Prompt assembly
			// reads inputs from the top and stops at a per-file cap, so
			// pointing every section at the whole file sent each one the same
			// opening 60 KB: a late chapter never saw its own words, and an
			// N-section book paid for N copies of its first pages.
			Inputs: []string{input},
			// Hashing the section's own text means editing one chapter
			// regenerates one section, not the whole book.
			Hash: hashText(s.Body),
			Meta: map[string]any{"parent": doc.Key, "heading": s.Title},
		})
	}
	return units, nil
}

// sectionInput names the file holding one section's text, writing it when the
// mapper is asked to stage.
//
// The name is derived from the parent's path, the section's position and its
// title, none of which feed the unit's key, slug or hash, so a unit's
// identity -- and therefore whether its page regenerates -- is untouched. The
// position keeps two sections with the same heading apart.
func (m *Mapper) sectionInput(root string, doc Doc, i int, s section) (string, error) {
	parent := strings.TrimLeft(strings.TrimSuffix(doc.Path, path.Ext(doc.Path)), "/")
	rel := path.Join(sectionsDir, parent, fmt.Sprintf("%02d-%s.md", i+1, fileSlug(s.Title)))
	// Paths come from connectors that stage under their own directory, but
	// this one is about to be written to, so a ".." that would climb out of
	// the staging root is refused rather than trusted.
	if !fs.ValidPath(rel) {
		return "", fmt.Errorf("docmap: section path %q escapes the staging root", rel)
	}
	if !m.StageSections {
		return rel, nil
	}

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("docmap: stage section %q of %s: %w", s.Title, doc.Path, err)
	}
	if err := os.WriteFile(full, []byte(s.Body), 0o644); err != nil {
		return "", fmt.Errorf("docmap: stage section %q of %s: %w", s.Title, doc.Path, err)
	}
	return rel, nil
}

// section is one top-level division of a document.
type section struct {
	Title string
	Body  string
}

var headingPattern = regexp.MustCompile(`(?m)^#\s+(.+)$`)

// split divides a long document at its top-level headings.
//
// Returns nil when the document is short enough to handle whole, or when it has
// fewer than two headings -- splitting on one heading just renames the document.
func (m *Mapper) split(doc Doc) []section {
	threshold := m.SplitMinBytes
	if threshold == 0 {
		threshold = SplitMinBytes
	}
	if len(doc.Text) < threshold {
		return nil
	}

	locs := headingPattern.FindAllStringSubmatchIndex(doc.Text, -1)
	if len(locs) < 2 {
		return nil
	}

	var out []section
	for i, loc := range locs {
		title := strings.TrimSpace(doc.Text[loc[2]:loc[3]])
		end := len(doc.Text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.TrimSpace(doc.Text[loc[0]:end])
		if title == "" || body == "" {
			continue
		}
		out = append(out, section{Title: title, Body: body})
	}
	return out
}

func (m *Mapper) render(wm *mapper.WorkspaceMap) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Document set\n\n%d unit(s).\n\n", len(wm.Units))

	for _, u := range wm.Units {
		if u.Kind == "doc" {
			fmt.Fprintf(&b, "- **%s** (`%s`)\n", u.Title, firstInput(u))
		}
	}
	return b.String()
}

func firstInput(u mapper.Unit) string {
	if len(u.Inputs) > 0 {
		return u.Inputs[0]
	}
	return ""
}

func (m *Mapper) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now().UTC()
}

var (
	nonSlug      = regexp.MustCompile(`[^a-z0-9]+`)
	dashCollapse = regexp.MustCompile(`-{2,}`)
)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = dashCollapse.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "document"
	}
	return s
}

func uniqueSlug(taken map[string]bool, base string) string {
	if !taken[base] {
		taken[base] = true
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}

// fileSlug is slugify bounded for use in a file name: a heading can be a whole
// sentence, and a name over the filesystem's limit would fail the staging.
func fileSlug(title string) string {
	s := slugify(title)
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashUnits digests the shape of the map, so the architecture synthesis is
// dirtied when documents are added or removed.
func hashUnits(units []mapper.Unit) string {
	h := sha256.New()
	for _, u := range units {
		fmt.Fprintf(h, "%s|%s|%s\n", u.Key, u.Slug, u.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}
