package repomap

import (
	"path"
	"strings"

	"golang.org/x/mod/modfile"
)

// GoModule is the parsed subset of a go.mod that matters for mapping.
type GoModule struct {
	Path string
	Dir  string // repo-relative directory containing the go.mod
	// Requires are direct external dependencies only; indirect ones are noise
	// in a wiki.
	Requires []string
	// LocalReplaces are replace directives pointing at a relative path. These
	// are real intra-repo edges -- it is how InfraFlux's shared/ is wired into
	// pilot/ and node/ -- so they become dependency edges rather than guesses.
	LocalReplaces []string
}

// ParseGoMod reads a go.mod body. dir is the repo-relative directory holding it.
func ParseGoMod(dir string, body []byte) (*GoModule, error) {
	f, err := modfile.Parse(path.Join(dir, "go.mod"), body, nil)
	if err != nil {
		return nil, err
	}

	m := &GoModule{Dir: dir}
	if f.Module != nil {
		m.Path = f.Module.Mod.Path
	}

	for _, r := range f.Require {
		if r.Indirect {
			continue
		}
		m.Requires = append(m.Requires, r.Mod.Path)
	}

	for _, r := range f.Replace {
		if isLocalPath(r.New.Path) {
			m.LocalReplaces = append(m.LocalReplaces, r.New.Path)
		}
	}
	return m, nil
}

// isLocalPath reports whether a replace target is a filesystem path rather than
// a module path.
func isLocalPath(p string) bool {
	return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") ||
		strings.HasPrefix(p, "/") || p == "." || p == ".."
}

// goPackageDirs returns the distinct directories containing .go files, relative
// to the module directory. These become the Packages list and are the basis for
// sub-partitioning a large single-module repo.
func goPackageDirs(moduleDir string, files []string) []string {
	seen := map[string]bool{}
	var out []string

	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		rel := relativeTo(moduleDir, f)
		if rel == "" {
			continue
		}
		dir := path.Dir(rel)
		if dir == "." {
			dir = "."
		}
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return out
}

// hasGoMain reports whether a file declares package main, which is how entry
// points are found. A regex over the package clause is sufficient and far
// cheaper than go/parser at this scale.
func hasGoMain(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "package ") {
			name := strings.TrimSpace(strings.TrimPrefix(trimmed, "package "))
			// Strip a trailing line comment: `package main // foo`
			if i := strings.Index(name, "//"); i >= 0 {
				name = strings.TrimSpace(name[:i])
			}
			return name == "main"
		}
	}
	return false
}
