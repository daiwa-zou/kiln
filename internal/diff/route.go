package diff

import (
	"path"
	"sort"
	"strings"
)

// Router maps changed paths to the units that must be regenerated.
type Router struct {
	// ModuleDirs maps a repo-relative module directory to its cache key.
	// Attribution uses the longest matching prefix, so a change under
	// apps/ripple is never credited to the root module.
	ModuleDirs map[string]Key
	// DocPaths maps a repo-relative document path to its cache key.
	DocPaths map[string]Key
	// Cosmetic paths are indexed but never trigger a rebuild on their own.
	Cosmetic func(path string) bool
}

// Plan is the routing result: which units are dirty and why.
type Plan struct {
	Dirty   []Key
	Reasons map[Key][]string
}

// IsEmpty reports whether any unit needs regeneration. An empty plan means the
// run completes with zero LLM calls and zero cost.
func (p Plan) IsEmpty() bool { return len(p.Dirty) == 0 }

// architecturalFiles change the shape of the whole workspace rather than one
// module, so they dirty the synthesis pages.
var architecturalFiles = map[string]bool{
	"docker-compose.yml":  true,
	"docker-compose.yaml": true,
	"Makefile":            true,
	"Taskfile.yml":        true,
	"Taskfile.yaml":       true,
	"Tiltfile":            true,
	"Procfile":            true,
	"Dockerfile":          true,
}

// manifestFiles declare module boundaries and dependencies. A change to one may
// mean modules appeared, vanished, or re-wired, so the architecture is dirty too.
var manifestFiles = map[string]bool{
	"go.mod":         true,
	"Cargo.toml":     true,
	"package.json":   true,
	"pyproject.toml": true,
}

// Route computes the dirty set for a change set.
func (r Router) Route(cs ChangeSet) Plan {
	reasons := map[Key][]string{}
	mark := func(k Key, reason string) {
		if k == "" {
			return
		}
		for _, existing := range reasons[k] {
			if existing == reason {
				return
			}
		}
		reasons[k] = append(reasons[k], reason)
	}

	if cs.FullRebuild {
		for _, k := range r.ModuleDirs {
			mark(k, "full rebuild")
		}
		for _, k := range r.DocPaths {
			mark(k, "full rebuild")
		}
		mark(ArchOverview, "full rebuild")
		return finish(reasons)
	}

	moduleDirs := sortedDirs(r.ModuleDirs)

	for _, c := range cs.Changes {
		base := path.Base(c.Path)

		// A document routes only to its own page: cheap and isolated.
		if k, ok := r.DocPaths[c.Path]; ok {
			mark(k, string(c.Kind)+" "+c.Path)
			continue
		}

		// Cosmetic files are indexed but do not justify an LLM call.
		if r.Cosmetic != nil && r.Cosmetic(c.Path) && !manifestFiles[base] && !architecturalFiles[base] {
			continue
		}

		if dir := longestPrefix(moduleDirs, c.Path); dir != "" {
			mark(r.ModuleDirs[dir], string(c.Kind)+" "+c.Path)
		}

		if manifestFiles[base] {
			// Boundaries or dependencies may have changed.
			mark(ArchOverview, "manifest changed: "+c.Path)
		}
		if architecturalFiles[base] || strings.HasSuffix(c.Path, ".proto") {
			mark(ArchOverview, "architecture input changed: "+c.Path)
		}
	}

	return finish(reasons)
}

func finish(reasons map[Key][]string) Plan {
	dirty := make([]Key, 0, len(reasons))
	for k := range reasons {
		sort.Strings(reasons[k])
		dirty = append(dirty, k)
	}
	// Architecture last: it summarizes the modules, so regenerating it after
	// them means it sees their current state.
	sort.Slice(dirty, func(i, j int) bool {
		ai, aj := dirty[i] == ArchOverview, dirty[j] == ArchOverview
		if ai != aj {
			return aj
		}
		return dirty[i] < dirty[j]
	})
	return Plan{Dirty: dirty, Reasons: reasons}
}

// sortedDirs returns module directories longest-first so the first match in
// longestPrefix is also the deepest.
func sortedDirs(dirs map[string]Key) []string {
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, path.Clean(d))
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

func longestPrefix(dirs []string, file string) string {
	file = path.Clean(file)
	for _, d := range dirs {
		if d == "." {
			// The root module matches anything, but only after every deeper
			// module has been given a chance.
			return d
		}
		if file == d || strings.HasPrefix(file, d+"/") {
			return d
		}
	}
	return ""
}

// StaleAdjacent returns pages that reference a dirty page but are not
// themselves regenerated.
//
// These are deliberately not added to the dirty set. Regenerating every page
// that merely links to a changed one cascades until the whole wiki rebuilds on
// any commit; instead their inbound links are re-validated in the post-pass.
func StaleAdjacent(related map[string][]string, dirtySlugs map[string]bool) []string {
	seen := map[string]bool{}
	var out []string

	for slug, refs := range related {
		if dirtySlugs[slug] {
			continue
		}
		for _, ref := range refs {
			if dirtySlugs[ref] && !seen[slug] {
				seen[slug] = true
				out = append(out, slug)
				break
			}
		}
	}

	sort.Strings(out)
	return out
}
