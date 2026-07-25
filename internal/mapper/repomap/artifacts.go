package repomap

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	protoPackage = regexp.MustCompile(`(?m)^\s*package\s+([A-Za-z0-9_.]+)\s*;`)
	protoService = regexp.MustCompile(`(?m)^\s*service\s+([A-Za-z0-9_]+)\s*\{`)
	protoMessage = regexp.MustCompile(`(?m)^\s*message\s+([A-Za-z0-9_]+)\s*\{`)

	// makeTarget matches a rule line while excluding variable assignments
	// (FOO := bar) and pattern rules.
	makeTarget = regexp.MustCompile(`(?m)^([A-Za-z0-9_][A-Za-z0-9_.-]*)\s*:(?:[^=]|$)`)
)

// ParseProto extracts a protobuf file's package, services, and messages by
// regex. Running protoc would require the full import graph and a toolchain,
// for information a wiki does not need.
func ParseProto(path string, body []byte) ProtoFile {
	text := stripBlockComments(string(body))

	pf := ProtoFile{Path: path}
	if m := protoPackage.FindStringSubmatch(text); m != nil {
		pf.Package = m[1]
	}
	for _, m := range protoService.FindAllStringSubmatch(text, -1) {
		pf.Services = append(pf.Services, m[1])
	}
	for _, m := range protoMessage.FindAllStringSubmatch(text, -1) {
		pf.Messages = append(pf.Messages, m[1])
	}
	return pf
}

// ParseMakefile extracts target names, skipping .PHONY bookkeeping and
// variable assignments.
func ParseMakefile(path string, body []byte) []BuildTarget {
	var out []BuildTarget
	seen := map[string]bool{}

	for _, m := range makeTarget.FindAllStringSubmatch(string(body), -1) {
		name := m[1]
		if strings.HasPrefix(name, ".") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, BuildTarget{File: path, Name: name})
	}
	return out
}

// ParseProcfile extracts process names.
func ParseProcfile(path string, body []byte) []BuildTarget {
	var out []BuildTarget
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, BuildTarget{File: path, Name: name})
		}
	}
	return out
}

// ParseCompose extracts services from a docker-compose file.
//
// Environment *values* are deliberately never captured -- only keys. A compose
// file routinely holds credentials, and a wiki is readable by everyone with
// access to the workspace.
func ParseCompose(body []byte) []ComposeService {
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")

	var (
		out       []ComposeService
		cur       *ComposeService
		inService bool
		section   string
	)

	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}

	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		trimmed := strings.TrimSpace(raw)

		// Top-level key.
		if indent == 0 {
			flush()
			inService = strings.HasPrefix(trimmed, "services:")
			continue
		}
		if !inService {
			continue
		}

		// A service name sits one level in and ends with a colon.
		if indent <= 2 && strings.HasSuffix(trimmed, ":") {
			flush()
			cur = &ComposeService{Name: strings.TrimSuffix(trimmed, ":")}
			section = ""
			continue
		}
		if cur == nil {
			continue
		}

		key, value, hasValue := strings.Cut(trimmed, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if strings.HasPrefix(trimmed, "- ") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			switch section {
			case "ports":
				cur.Ports = append(cur.Ports, unquote(item))
			case "depends_on":
				cur.DependsOn = append(cur.DependsOn, unquote(item))
			case "environment":
				// "- KEY=value": keep the key, discard the value.
				if k, _, ok := strings.Cut(item, "="); ok {
					cur.EnvKeys = append(cur.EnvKeys, strings.TrimSpace(k))
				} else {
					cur.EnvKeys = append(cur.EnvKeys, item)
				}
			}
			continue
		}

		if hasValue && value == "" {
			section = key
			continue
		}
		section = ""

		switch key {
		case "image":
			cur.Image = unquote(value)
		case "build":
			cur.Build = unquote(value)
		default:
			// "KEY: value" inside an environment mapping.
			if section == "" && strings.Contains(trimmed, ":") && indent >= 6 {
				continue
			}
		}
	}
	flush()

	return out
}

func unquote(s string) string {
	return strings.Trim(strings.TrimSpace(s), `"'`)
}

// stripBlockComments removes /* ... */ so commented-out declarations are not
// mistaken for real ones.
func stripBlockComments(s string) string {
	for {
		start := strings.Index(s, "/*")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "*/")
		if end < 0 {
			return s[:start]
		}
		s = s[:start] + s[start+end+2:]
	}
}

// DocKind classifies a documentation file.
func DocKind(path string) string {
	base := strings.ToLower(filepath.Base(path))
	lower := strings.ToLower(path)

	switch {
	case base == "claude.md" || base == "agents.md":
		return "claude-md"
	case strings.HasPrefix(base, "readme"):
		return "readme"
	case strings.HasPrefix(base, "changelog"):
		return "changelog"
	case strings.HasPrefix(base, "contributing"):
		return "contributing"
	case strings.Contains(lower, "adr"):
		return "adr"
	default:
		return "doc"
	}
}

// DocTitle returns a document's first H1, falling back to its filename.
func DocTitle(path string, body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "# "); ok {
			if title := strings.TrimSpace(after); title != "" {
				return title
			}
		}
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}
