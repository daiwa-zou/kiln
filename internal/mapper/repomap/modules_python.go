package repomap

import (
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// PythonManifest is the parsed subset of a pyproject.toml that matters for
// mapping: identity, dependencies, and console scripts. Both PEP 621
// ([project]) and Poetry ([tool.poetry]) layouts are read, because the wild
// contains plenty of each.
type PythonManifest struct {
	Name    string
	Deps    []string
	Scripts []string // console-script names: how the package is executed
}

type rawPyproject struct {
	Project *struct {
		Name         string            `toml:"name"`
		Dependencies []string          `toml:"dependencies"`
		Scripts      map[string]string `toml:"scripts"`
	} `toml:"project"`
	Tool struct {
		Poetry *struct {
			Name         string                    `toml:"name"`
			Dependencies map[string]toml.Primitive `toml:"dependencies"`
			Scripts      map[string]string         `toml:"scripts"`
		} `toml:"poetry"`
	} `toml:"tool"`
}

// ParsePyproject reads a pyproject.toml body.
func ParsePyproject(body []byte) (*PythonManifest, error) {
	var raw rawPyproject
	if _, err := toml.Decode(string(body), &raw); err != nil {
		return nil, err
	}

	m := &PythonManifest{}
	if raw.Project != nil {
		m.Name = raw.Project.Name
		for _, d := range raw.Project.Dependencies {
			if name := pythonDepName(d); name != "" {
				m.Deps = append(m.Deps, name)
			}
		}
		for name := range raw.Project.Scripts {
			m.Scripts = append(m.Scripts, name)
		}
	}
	// Poetry fills whatever PEP 621 metadata did not provide, so a hybrid
	// file (PEP 621 name, poetry deps) still maps fully.
	if p := raw.Tool.Poetry; p != nil {
		if m.Name == "" {
			m.Name = p.Name
		}
		if len(m.Deps) == 0 {
			for name := range p.Dependencies {
				// Poetry lists the interpreter as a dependency; it is a
				// runtime requirement, not a library edge.
				if strings.EqualFold(name, "python") {
					continue
				}
				m.Deps = append(m.Deps, name)
			}
		}
		if len(m.Scripts) == 0 {
			for name := range p.Scripts {
				m.Scripts = append(m.Scripts, name)
			}
		}
	}

	sort.Strings(m.Deps)
	sort.Strings(m.Scripts)
	return m, nil
}

// pythonDepName strips a PEP 508 requirement down to its package name:
// "requests[socks]>=2.28,<3; python_version>='3.8'" -> "requests".
func pythonDepName(req string) string {
	req = strings.TrimSpace(req)
	for i, r := range req {
		isName := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !isName {
			return req[:i]
		}
	}
	return req
}
