package mapper

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"time"
)

// MergeKind marks a map assembled from more than one connector.
const MergeKind = "mixed"

// Merge combines the maps produced by several connectors into the single map a
// build runs against.
//
// This is what makes a workspace source-agnostic in practice rather than only
// in the interface: a repository and a folder of PDFs each get the mapper that
// understands them, and the result is one unit list, one hash, and one wiki
// whose pages can link across the boundary. Without it each connector would
// need its own run and cross-source wikilinks could never resolve, because the
// two halves would never be in scope together.
//
// Root comes from the caller because the merged map has no single root: units
// carry absolute or connector-relative paths of their own, and inventing a
// common ancestor would silently rewrite them.
//
// A key collision is an error rather than a silent overwrite -- two producers
// for one page means one of them would be discarded on every run, and the loss
// would show up as a page that mysteriously never updates.
func Merge(root string, maps ...*WorkspaceMap) (*WorkspaceMap, error) {
	out := &WorkspaceMap{SchemaVersion: 1, Root: root}

	var (
		kinds     []string
		summaries []string
		seen      = map[string]string{} // unit key -> the map kind that claimed it
	)

	for _, m := range maps {
		if m == nil {
			continue
		}
		if m.Kind != "" && !slices.Contains(kinds, m.Kind) {
			kinds = append(kinds, m.Kind)
		}
		// The newest generation time wins, so a merged map is never reported as
		// fresher than its stalest half.
		if m.GeneratedAt.After(out.GeneratedAt) {
			out.GeneratedAt = m.GeneratedAt
		}

		for _, u := range m.Units {
			if prev, dup := seen[u.Key]; dup {
				return nil, &DuplicateKeyError{Key: u.Key, First: prev, Second: m.Kind}
			}
			seen[u.Key] = m.Kind
			out.Units = append(out.Units, u)
		}
		out.Edges = append(out.Edges, m.Edges...)

		if s := strings.TrimSpace(m.Summary); s != "" {
			summaries = append(summaries, s)
		}
	}

	switch len(kinds) {
	case 0:
		out.Kind = MergeKind
	case 1:
		// A single-connector workspace should not be labelled "mixed"; that
		// would misreport the common case to make the rare one uniform.
		out.Kind = kinds[0]
	default:
		out.Kind = MergeKind
	}

	if out.GeneratedAt.IsZero() {
		out.GeneratedAt = time.Now().UTC()
	}

	// Units are sorted so the merged order does not depend on which connector
	// the caller happened to sync first. The unit hash below depends on this.
	sort.SliceStable(out.Units, func(i, j int) bool { return out.Units[i].Key < out.Units[j].Key })

	out.Summary = strings.Join(summaries, "\n\n")
	out.Hash = hashUnits(out.Units)
	return out, nil
}

// DuplicateKeyError reports two mappers claiming the same unit key.
type DuplicateKeyError struct {
	Key           string
	First, Second string // the mapper kinds that both claimed it
}

func (e *DuplicateKeyError) Error() string {
	return "mapper: unit key " + e.Key + " claimed by both the " +
		e.First + " and " + e.Second + " mappers"
}

// hashUnits digests the merged shape.
//
// Keyed on unit key and content hash only: a unit that moved in the list has
// not changed, and treating it as changed would spend a model call to rewrite
// an identical page.
func hashUnits(units []Unit) string {
	h := sha256.New()
	for _, u := range units {
		h.Write([]byte(u.Key))
		h.Write([]byte{0})
		h.Write([]byte(u.Hash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
