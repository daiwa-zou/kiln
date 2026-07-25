package repomap

import (
	"fmt"
	"sort"
	"strings"
)

// Render produces the human- and agent-readable form of the map.
//
// This is given to the agent as grounding, so it states only what was
// extracted deterministically. Everything here is a fact from a manifest, the
// filesystem, or git history -- never an inference.
func (rm *RepoMap) Render() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Repository map: %s\n\n", rm.Slug)

	if rm.Git != nil && rm.Git.HeadSHA != "" {
		fmt.Fprintf(&b, "- Commit: `%s` on `%s`\n", shortSHA(rm.Git.HeadSHA), rm.Git.Branch)
		fmt.Fprintf(&b, "- History: %d commits\n", rm.Git.CommitCount)
		if rm.Git.Dirty {
			b.WriteString("- Working tree: has uncommitted changes\n")
		}
	} else {
		// Two of the directories this was built against have no .git, so this
		// is a supported state rather than a problem to report.
		b.WriteString("- Not a git repository; change detection uses content hashes\n")
	}

	if len(rm.Languages) > 0 {
		b.WriteString("- Languages: ")
		b.WriteString(strings.Join(rm.topLanguages(6), ", "))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	renderModules(&b, rm)
	renderEntryPoints(&b, rm)
	renderProtos(&b, rm)
	renderServices(&b, rm)
	renderBuildTargets(&b, rm)
	renderDocs(&b, rm)

	return b.String()
}

func renderModules(b *strings.Builder, rm *RepoMap) {
	if len(rm.Modules) == 0 {
		return
	}
	b.WriteString("## Modules\n\n")
	b.WriteString("| Module | Directory | Kind | Files | LOC | Depends on |\n")
	b.WriteString("|---|---|---|---|---|---|\n")

	for _, m := range rm.Modules {
		name := m.Name
		if m.Parent != "" {
			name = "  ↳ " + name
		}
		deps := "—"
		if len(m.DependsOn) > 0 {
			deps = strings.Join(m.DependsOn, ", ")
		}
		note := ""
		if m.Empty {
			note = " *(empty)*"
		}
		fmt.Fprintf(b, "| %s%s | `%s` | %s | %d | %d | %s |\n",
			name, note, m.Dir, m.Kind, m.FileCount, m.LOC, deps)
	}
	b.WriteString("\n")
}

func renderEntryPoints(b *strings.Builder, rm *RepoMap) {
	if len(rm.EntryPoints) == 0 {
		return
	}
	b.WriteString("## Entry points\n\n")
	for _, e := range rm.EntryPoints {
		fmt.Fprintf(b, "- `%s` — %s `%s`\n", e.Path, e.Kind, e.Name)
	}
	b.WriteString("\n")
}

func renderProtos(b *strings.Builder, rm *RepoMap) {
	if len(rm.Protos) == 0 {
		return
	}
	b.WriteString("## Protobuf definitions\n\n")
	for _, p := range rm.Protos {
		fmt.Fprintf(b, "- `%s`", p.Path)
		if p.Package != "" {
			fmt.Fprintf(b, " (package `%s`)", p.Package)
		}
		if len(p.Services) > 0 {
			fmt.Fprintf(b, " — services: %s", strings.Join(p.Services, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func renderServices(b *strings.Builder, rm *RepoMap) {
	if len(rm.Services) == 0 {
		return
	}
	b.WriteString("## Compose services\n\n")
	for _, s := range rm.Services {
		fmt.Fprintf(b, "- **%s**", s.Name)
		if s.Image != "" {
			fmt.Fprintf(b, " — image `%s`", s.Image)
		} else if s.Build != "" {
			fmt.Fprintf(b, " — built from `%s`", s.Build)
		}
		if len(s.DependsOn) > 0 {
			fmt.Fprintf(b, ", depends on %s", strings.Join(s.DependsOn, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// renderBuildTargets groups by source file.
//
// A flat list conflates unrelated things: watchtower's Procfile process names
// and its Makefile targets read as one set, and a monorepo's per-app Makefiles
// appear to define the same target repeatedly.
func renderBuildTargets(b *strings.Builder, rm *RepoMap) {
	if len(rm.BuildTargets) == 0 {
		return
	}

	byFile := map[string][]string{}
	var order []string
	for _, t := range rm.BuildTargets {
		if _, seen := byFile[t.File]; !seen {
			order = append(order, t.File)
		}
		byFile[t.File] = append(byFile[t.File], "`"+t.Name+"`")
	}
	sort.Strings(order)

	b.WriteString("## Build targets\n\n")
	for _, file := range order {
		fmt.Fprintf(b, "- `%s`: %s\n", file, strings.Join(byFile[file], ", "))
	}
	b.WriteString("\n")
}

func renderDocs(b *strings.Builder, rm *RepoMap) {
	if len(rm.Docs) == 0 {
		return
	}
	b.WriteString("## Documentation\n\n")
	for _, d := range rm.Docs {
		fmt.Fprintf(b, "- `%s` — %s: %s\n", d.Path, d.Kind, d.Title)
	}
	b.WriteString("\n")
}

// topLanguages returns the n largest languages by line count.
func (rm *RepoMap) topLanguages(n int) []string {
	type entry struct {
		name string
		stat LangStat
	}
	entries := make([]entry, 0, len(rm.Languages))
	for name, stat := range rm.Languages {
		entries = append(entries, entry{name, stat})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].stat.LOC != entries[j].stat.LOC {
			return entries[i].stat.LOC > entries[j].stat.LOC
		}
		return entries[i].name < entries[j].name
	})

	if len(entries) > n {
		entries = entries[:n]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = fmt.Sprintf("%s (%d)", e.name, e.stat.LOC)
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
