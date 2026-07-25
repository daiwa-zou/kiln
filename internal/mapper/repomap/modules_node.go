package repomap

import (
	"encoding/json"
	"sort"
)

// NodeManifest is the parsed subset of a package.json that matters for mapping.
type NodeManifest struct {
	Name       string
	Private    bool
	Bins       []string
	Scripts    []string
	Deps       []string
	Workspaces []string
	LocalDeps  []string // file:/workspace: dependencies -- intra-repo edges
}

type rawNode struct {
	Name            string            `json:"name"`
	Private         bool              `json:"private"`
	Bin             json.RawMessage   `json:"bin"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	// Workspaces is either an array or an object with a "packages" array.
	Workspaces json.RawMessage `json:"workspaces"`
}

// ParsePackageJSON reads a package.json body.
func ParsePackageJSON(body []byte) (*NodeManifest, error) {
	var raw rawNode
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	m := &NodeManifest{Name: raw.Name, Private: raw.Private}

	// bin is either a string (single binary named after the package) or an
	// object mapping names to paths.
	if len(raw.Bin) > 0 {
		var single string
		if err := json.Unmarshal(raw.Bin, &single); err == nil && single != "" {
			m.Bins = append(m.Bins, raw.Name)
		} else {
			var multi map[string]string
			if err := json.Unmarshal(raw.Bin, &multi); err == nil {
				for name := range multi {
					m.Bins = append(m.Bins, name)
				}
			}
		}
	}

	for name := range raw.Scripts {
		m.Scripts = append(m.Scripts, name)
	}

	for name, version := range raw.Dependencies {
		m.Deps = append(m.Deps, name)
		if isLocalNodeDep(version) {
			m.LocalDeps = append(m.LocalDeps, name)
		}
	}
	// Dev dependencies are recorded as deps too: a wiki reader cares that a
	// module uses vitest, not which stanza declares it.
	for name, version := range raw.DevDependencies {
		m.Deps = append(m.Deps, name)
		if isLocalNodeDep(version) {
			m.LocalDeps = append(m.LocalDeps, name)
		}
	}

	if len(raw.Workspaces) > 0 {
		var arr []string
		if err := json.Unmarshal(raw.Workspaces, &arr); err == nil {
			m.Workspaces = arr
		} else {
			var obj struct {
				Packages []string `json:"packages"`
			}
			if err := json.Unmarshal(raw.Workspaces, &obj); err == nil {
				m.Workspaces = obj.Packages
			}
		}
	}

	sort.Strings(m.Bins)
	sort.Strings(m.Scripts)
	sort.Strings(m.Deps)
	sort.Strings(m.LocalDeps)
	sort.Strings(m.Workspaces)
	m.Deps = dedupeSorted(m.Deps)
	m.LocalDeps = dedupeSorted(m.LocalDeps)
	return m, nil
}

func isLocalNodeDep(version string) bool {
	return hasPrefix(version, "file:") || hasPrefix(version, "workspace:") || hasPrefix(version, "link:")
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func sortStrings(s []string) { sort.Strings(s) }

// dedupeSorted removes adjacent duplicates from an already-sorted slice.
func dedupeSorted(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
