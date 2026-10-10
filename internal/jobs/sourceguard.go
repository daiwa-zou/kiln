package jobs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

// errSourcesModified marks a unit whose agent left the source tree different
// from how it found it. The sandbox is meant to make that impossible (the CLI
// runs --restricted with an edit deny on the sources), so seeing it means a
// layer failed, and the unit is failed rather than trusted.
var errSourcesModified = errors.New("the agent modified the source tree")

// snapshotEntryCap bounds the entries one snapshot records, and so its cost: at
// most this many lstat calls, a few tens of milliseconds, per look. A variable
// only so a test can reach the cap without creating twenty thousand files.
var snapshotEntryCap = 20_000

// sourceEntry is what is compared for one path.
//
// A directory's size is filesystem-defined noise, and its mtime moves whenever
// anything in it is created or removed -- including by the operating system,
// which is not an agent. Both are left out for a directory that is listed or
// walked, because what was added or removed shows as entries of its own. They
// are kept for a directory that is recorded and not entered (see skipDescent),
// where the mtime is the only sign of a new file inside.
type sourceEntry struct {
	mode  fs.FileMode
	size  int64
	mtime int64
	// listed marks entries found by listing or walking a directory, as opposed
	// to named outright (the root, the unit's inputs and its directory). Listings
	// are what the cap cuts off, so when a snapshot was truncated only a named
	// entry's appearance or disappearance proves anything.
	listed bool
}

type sourceSnapshot struct {
	root      string
	entries   map[string]sourceEntry
	truncated bool
}

// skipDescent names directories recorded but never entered by the walk: they
// are large, machine-written and not what a unit documents. The directory
// entry itself is still compared, so a file created directly inside one moves
// its mtime and is seen.
var skipDescent = map[string]bool{".git": true, "node_modules": true}

// ignoredNames are files the operating system, not an agent, creates.
var ignoredNames = map[string]bool{".DS_Store": true}

// snapshotSources fingerprints the part of the source tree a unit's agent is
// pointed at. An empty root (the API runner's units, which have no checkout)
// yields an empty snapshot that never differs from another.
func snapshotSources(root string, unit mapper.Unit) *sourceSnapshot {
	s := &sourceSnapshot{root: root, entries: map[string]sourceEntry{}}
	if root == "" {
		return s
	}

	// Named entries first, so the cap can never push out the files the agent was
	// told to read: only the listings below it compete for what room is left.
	var inputs []string
	for _, in := range unit.Inputs {
		if rel := filepath.Clean(in); filepath.IsLocal(rel) {
			inputs = append(inputs, rel)
		}
	}
	// The unit's whole directory, but never the whole repository: a unit with no
	// directory (documents, the architecture synthesis) gets the root's flat
	// listing and nothing more.
	dir := ""
	if d := filepath.Clean(unit.Dir); unit.Dir != "" && d != "." && filepath.IsLocal(d) {
		dir = d
	}

	s.record(".")
	for _, in := range inputs {
		s.record(in)
	}
	if dir != "" {
		s.record(dir)
	}

	s.listFlat(".")
	listed := map[string]bool{".": true}
	for _, in := range inputs {
		for d := filepath.Dir(in); !listed[d]; d = filepath.Dir(d) {
			listed[d] = true
			s.listFlat(d)
		}
	}

	for _, in := range inputs {
		// A unit with no entry points is given its directory as its input.
		if fi, err := os.Lstat(filepath.Join(root, in)); err == nil && fi.IsDir() {
			s.walk(in)
		}
	}
	if dir != "" {
		s.walk(dir)
	}
	return s
}

func (s *sourceSnapshot) full() bool { return len(s.entries) >= snapshotEntryCap }

// record lstats one path and stores it, quietly skipping what is missing: a
// path absent now but present later is a change, and shows as an addition.
func (s *sourceSnapshot) record(rel string) {
	fi, err := os.Lstat(filepath.Join(s.root, rel))
	if err != nil {
		return
	}
	s.add(rel, fi, false)
}

func (s *sourceSnapshot) add(rel string, fi fs.FileInfo, listed bool) {
	if ignoredNames[fi.Name()] {
		return
	}
	if s.full() {
		s.truncated = true
		return
	}
	e := sourceEntry{mode: fi.Mode(), listed: listed}
	if !fi.IsDir() {
		e.size = fi.Size()
		e.mtime = fi.ModTime().UnixNano()
	} else if skipDescent[fi.Name()] {
		e.mtime = fi.ModTime().UnixNano()
	}
	s.entries[rel] = e
}

// listFlat records the direct entries of one directory.
func (s *sourceSnapshot) listFlat(rel string) {
	des, err := os.ReadDir(filepath.Join(s.root, rel))
	if err != nil {
		return
	}
	for _, de := range des {
		if fi, err := de.Info(); err == nil {
			s.addListed(filepath.Join(rel, de.Name()), fi)
		}
	}
}

// addListed stores an entry found by listing a directory. One already named
// outright keeps that status: it is the stronger claim.
func (s *sourceSnapshot) addListed(rel string, fi fs.FileInfo) {
	if _, named := s.entries[rel]; !named {
		s.add(rel, fi, true)
	}
}

// walk records everything beneath rel, in lexical order, stopping at the cap.
func (s *sourceSnapshot) walk(rel string) {
	start := filepath.Join(s.root, rel)
	_ = filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if s.full() {
			s.truncated = true
			return fs.SkipAll
		}
		// An entry that vanishes between the listing and the stat is simply not
		// recorded; the comparison will see it gone.
		fi, ierr := d.Info()
		r, rerr := filepath.Rel(s.root, path)
		if ierr == nil && rerr == nil {
			s.addListed(r, fi)
		}
		if d.IsDir() && path != start && skipDescent[d.Name()] {
			return fs.SkipDir
		}
		return nil
	})
}

// changesTo lists, sorted and human-readable, how after differs from s.
//
// When either snapshot hit the cap, the listings covered a different prefix of
// the tree each time, so a listed entry found in one and not the other proves
// nothing and is not reported. Modifications are, and so is the appearance or
// disappearance of a named entry (the root, an input, the unit's directory),
// which the cap never displaces.
func (s *sourceSnapshot) changesTo(after *sourceSnapshot) []string {
	partial := s.truncated || after.truncated
	var out []string
	for rel, was := range s.entries {
		now, ok := after.entries[rel]
		switch {
		case !ok:
			if !partial || !was.listed {
				out = append(out, "removed "+rel)
			}
		case now.mode != was.mode || now.size != was.size || now.mtime != was.mtime:
			out = append(out, "modified "+rel)
		}
	}
	for rel, now := range after.entries {
		if _, ok := s.entries[rel]; !ok && (!partial || !now.listed) {
			out = append(out, "added "+rel)
		}
	}
	sort.Strings(out)
	return out
}

// sourceGuard is one unit's before-snapshot and what it needs to look again.
//
// It exists because the sandbox is configuration handed to another program: it
// is believed, not proven, to refuse an edit beneath the sources, and this is
// the check that does not depend on that belief. It compares the sources
// before and after the agent has run and fails the unit if they differ. It
// never repairs anything: for a local-path connector the tree is the operator's
// real checkout, and kiln putting files back would be a second, unreviewed
// write to it.
//
// What it looks at is bounded on purpose. A unit of a large repository is a
// small part of it, there is one guard per unit, and units run in parallel, so
// walking the whole tree each time would cost more than the model call it
// protects on a big monorepo. (Git is no help either: `git status` reports a
// file already modified in a dirty checkout as the same line before and after
// an edit, and says nothing about ignored files.) The snapshot covers:
//
//   - the root's own entries, flat: a stray file written beside the sources,
//     which is how the agent once misplaced a whole set of pages;
//   - every input the unit names, and the entries of each directory above it:
//     the files the agent is told to read, and the places a neighbour would be
//     created;
//   - the unit's own directory, recursively: the module it is documenting.
//
// The trade-off is that a write to an unrelated file elsewhere in a large tree
// goes unseen. That is accepted: the edit deny and --restricted are what
// prevent it, and this check detects the failures that matter -- an edit to
// what the agent was reading, a file created where its pages should not be.
//
// A tree that a person edits while the build runs reads as modified too. The
// check cannot tell them apart and does not try: the unit's material is stale
// by then regardless, and failing it leaves it to be rebuilt on the next run.
type sourceGuard struct {
	root   string
	unit   mapper.Unit
	before *sourceSnapshot
}

// newSourceGuard snapshots the unit's sources. Nil when there is nothing to
// guard, which every method tolerates.
func newSourceGuard(root string, unit mapper.Unit) *sourceGuard {
	if root == "" {
		return nil
	}
	return &sourceGuard{root: root, unit: unit, before: snapshotSources(root, unit)}
}

// verify looks again and returns errSourcesModified, wrapped with what changed,
// if the tree is not as it was. It can be called after every agent call; each
// call compares against the original snapshot, not the previous look.
func (g *sourceGuard) verify() (changes []string, err error) {
	if g == nil {
		return nil, nil
	}
	changes = g.before.changesTo(snapshotSources(g.root, g.unit))
	if len(changes) == 0 {
		return nil, nil
	}
	const shown = 5
	list := strings.Join(changes[:min(len(changes), shown)], ", ")
	if len(changes) > shown {
		list += fmt.Sprintf(", and %d more", len(changes)-shown)
	}
	return changes, fmt.Errorf("%w under %s (%s); its pages were not imported and nothing was restored",
		errSourcesModified, g.root, list)
}
