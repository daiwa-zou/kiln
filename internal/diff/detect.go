package diff

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

// Empty reports whether anything changed.
func (cs ChangeSet) Empty() bool {
	return !cs.FullRebuild && len(cs.Changes) == 0
}
