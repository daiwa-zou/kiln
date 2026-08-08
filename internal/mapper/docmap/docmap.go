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
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

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
}

// Mapper partitions documents into units.
type Mapper struct {
	// SplitMinBytes overrides the default split threshold.
	SplitMinBytes int
	// Now is injectable so tests can pin GeneratedAt.
	Now func() time.Time
}

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
		units := m.unitsFor(doc, taken)
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

func (m *Mapper) unitsFor(doc Doc, taken map[string]bool) []mapper.Unit {
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
		return []mapper.Unit{whole}
	}

	units := []mapper.Unit{whole}
	for _, s := range sections {
		units = append(units, mapper.Unit{
			Key:   doc.Key + "#" + slugify(s.Title),
			Kind:  "doc-section",
			Slug:  uniqueSlug(taken, slugify(title+"-"+s.Title)),
			Title: s.Title,
			// The section points at its parent document: there is no separate
			// file on disk to read, only a span of one.
			Inputs: []string{doc.Path},
			// Hashing the section's own text means editing one chapter
			// regenerates one section, not the whole book.
			Hash: hashText(s.Body),
			Meta: map[string]any{"parent": doc.Key, "heading": s.Title},
		})
	}
	return units
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
