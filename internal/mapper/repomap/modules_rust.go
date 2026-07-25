package repomap

import (
	"strings"

	"github.com/BurntSushi/toml"
)

// CargoManifest is the parsed subset of a Cargo.toml that matters for mapping.
type CargoManifest struct {
	Name            string
	Version         string
	Bins            []CargoBin
	HasLib          bool
	Deps            []string
	LocalDeps       []string // path = "..." dependencies: intra-repo edges
	Members         []string // workspace members, possibly with globs
	IsWorkspaceOnly bool     // a virtual manifest with no [package]
}

// CargoBin is a [[bin]] target. sawmill has two (sawmill, feller), which is why
// entry points cannot be assumed to be one per package.
type CargoBin struct {
	Name string
	Path string
}

type rawCargo struct {
	Package *struct {
		Name    string `toml:"name"`
		Version string `toml:"version"`
	} `toml:"package"`
	Bin []struct {
		Name string `toml:"name"`
		Path string `toml:"path"`
	} `toml:"bin"`
	Lib *struct {
		Name string `toml:"name"`
		Path string `toml:"path"`
	} `toml:"lib"`
	Dependencies map[string]toml.Primitive `toml:"dependencies"`
	Workspace    *struct {
		Members []string `toml:"members"`
	} `toml:"workspace"`
}

// ParseCargoToml reads a Cargo.toml body.
func ParseCargoToml(body []byte) (*CargoManifest, error) {
	var raw rawCargo
	md, err := toml.Decode(string(body), &raw)
	if err != nil {
		return nil, err
	}

	m := &CargoManifest{HasLib: raw.Lib != nil}
	if raw.Package != nil {
		m.Name = raw.Package.Name
		m.Version = raw.Package.Version
	} else {
		m.IsWorkspaceOnly = true
	}

	for _, b := range raw.Bin {
		m.Bins = append(m.Bins, CargoBin{Name: b.Name, Path: b.Path})
	}
	if raw.Workspace != nil {
		m.Members = raw.Workspace.Members
	}

	for name, prim := range raw.Dependencies {
		m.Deps = append(m.Deps, name)

		// A table-valued dependency may carry `path = "..."`, which is an
		// intra-repo edge rather than an external crate.
		var table map[string]any
		if err := md.PrimitiveDecode(prim, &table); err != nil {
			continue
		}
		if p, ok := table["path"].(string); ok && p != "" {
			m.LocalDeps = append(m.LocalDeps, p)
		}
	}

	sortStrings(m.Deps)
	sortStrings(m.LocalDeps)
	return m, nil
}

// rustModPaths reports the distinct directories under src/ holding .rs files,
// used for sub-partitioning a large crate.
func rustModPaths(moduleDir string, files []string) []string {
	seen := map[string]bool{}
	var out []string

	for _, f := range files {
		if !strings.HasSuffix(f, ".rs") {
			continue
		}
		rel := relativeTo(moduleDir, f)
		if rel == "" || !strings.HasPrefix(rel, "src/") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(rel, "src/"), "/")
		if len(parts) < 2 {
			continue // a file directly in src/, not a module directory
		}
		dir := "src/" + parts[0]
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return out
}
