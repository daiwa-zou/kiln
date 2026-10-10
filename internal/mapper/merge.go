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
// understands them, and the result is one unit list and one wiki
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
		kinds      []string
		summaries  []string
		archHashes []string
		seen       = map[string]string{} // unit key -> the map kind that claimed it
	)

	for _, m := range maps {
		if m == nil {
			continue
		}
		if m.ArchHash != "" {
			archHashes = append(archHashes, m.ArchHash)
		}
		if m.Kind != "" && !slices.Contains(kinds, m.Kind) {
			kinds = append(kinds, m.Kind)
		}
		// The newest generation time wins: the merged map is as fresh as its
		// freshest part.
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
	// the caller happened to sync first.
	sort.SliceStable(out.Units, func(i, j int) bool { return out.Units[i].Key < out.Units[j].Key })

	out.Summary = strings.Join(summaries, "\n\n")
	out.ArchHash = mergeArchHashes(archHashes)
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

// mergeArchHashes carries the architecture digest through a merge.
//
// Only the repository mapper produces one, so the usual input is a single
// value and it passes through untouched: that is what keeps a repository
// merged with any number of documents on the same digest as the repository
// alone, and what the architecture synthesis's gate depends on. Several
// values (two repositories) combine order-independently, so reordering the
// connectors does not look like a structural change.
func mergeArchHashes(hashes []string) string {
	switch len(hashes) {
	case 0:
		return ""
	case 1:
		return hashes[0]
	}
	sorted := slices.Clone(hashes)
	slices.Sort(sorted)
	h := sha256.New()
	for _, s := range sorted {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
