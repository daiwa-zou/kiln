package diff

import "sort"

// ChangeKind classifies what happened to a path between two runs.
type ChangeKind string

const (
	Added    ChangeKind = "added"
	Modified ChangeKind = "modified"
	Deleted  ChangeKind = "deleted"
)

// Change is one path that differs from the last run.
type Change struct {
	Path string
	Kind ChangeKind
}

// ChangeSet is the full difference between two runs, however it was derived:
// a git diff for repositories, or a content-hash comparison for everything
// else. Both produce this same type, so routing does not care which was used.
type ChangeSet struct {
	// FromRef and ToRef are set for git-derived sets and empty otherwise.
	FromRef string
	ToRef   string
	Changes []Change
	// FullRebuild is set when no usable baseline existed -- a first run, a
	// force-push, or an unreachable base commit -- and everything is dirty.
	FullRebuild bool
}

// HashDiff compares two path->hash maps and returns the changes between them.
// This is the path for sources with no revision history: uploads, web
// snapshots, and directories that are not git repositories.
func HashDiff(before, after map[string]string) ChangeSet {
	var out []Change

	for path, newHash := range after {
		oldHash, existed := before[path]
		switch {
		case !existed:
			out = append(out, Change{Path: path, Kind: Added})
		case oldHash != newHash:
			out = append(out, Change{Path: path, Kind: Modified})
		}
	}

	for path := range before {
		if _, stillThere := after[path]; !stillThere {
			out = append(out, Change{Path: path, Kind: Deleted})
		}
	}

	// Sorted so a run's plan and its log entry are reproducible.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})

	return ChangeSet{Changes: out, FullRebuild: len(before) == 0 && len(after) > 0}
}

// Empty reports whether anything changed.
func (cs ChangeSet) Empty() bool {
	return !cs.FullRebuild && len(cs.Changes) == 0
}

// Paths returns the changed paths for a given kind.
func (cs ChangeSet) Paths(kind ChangeKind) []string {
	var out []string
	for _, c := range cs.Changes {
		if c.Kind == kind {
			out = append(out, c.Path)
		}
	}
	return out
}

// Counts summarizes the set, for log entries and run rows.
func (cs ChangeSet) Counts() (added, modified, deleted int) {
	for _, c := range cs.Changes {
		switch c.Kind {
		case Added:
			added++
		case Modified:
			modified++
		case Deleted:
			deleted++
		}
	}
	return added, modified, deleted
}
