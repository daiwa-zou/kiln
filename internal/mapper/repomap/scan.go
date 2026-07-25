package repomap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daiwa-zou/kiln/internal/mapper"
)

// SubPartitionMinLOC is the size above which a single-manifest module is split
// by its top-level package directories.
//
// watchtower is one go.mod but really five services plus cmd, pkg, and proto
// trees. Without splitting, a 15k-LOC repository collapses into one useless
// page; with it, each service gets its own.
const SubPartitionMinLOC = 2000

// subPartitionDirs are the conventional top-level groupings worth splitting on.
var subPartitionDirs = []string{"cmd", "internal", "pkg", "apps", "services", "src", "lib"}

// Scanner builds a RepoMap from a directory.
type Scanner struct {
	Walk WalkOptions
	// Slug names the workspace. It must be supplied by the caller because the
	// scan root is a per-run checkout directory whose name varies; deriving a
	// slug from it would make every run look different from the last.
	Slug string
	// Now is injectable so tests can pin GeneratedAt.
	Now func() time.Time
}

// RootModuleSlug is the stable slug for a module at the repository root.
//
// Emphatically not the directory's basename: the scan root is a temporary
// checkout, so a path-derived slug would change every run, the module hash
// would change with it, and nothing would ever be skipped.
const RootModuleSlug = "root"

// Kind implements mapper.Mapper.
func (s *Scanner) Kind() string { return "git" }

// Map implements mapper.Mapper, converting a scan into the partitioning the
// pipeline consumes.
func (s *Scanner) Map(ctx context.Context, set *mapper.SourceSet) (*mapper.WorkspaceMap, error) {
	rm, err := s.Scan(ctx, set.Root)
	if err != nil {
		return nil, err
	}
	return rm.ToWorkspaceMap(), nil
}

// Scan walks a repository and produces its map. It performs no LLM calls and
// is fast enough to run on every build.
func (s *Scanner) Scan(ctx context.Context, root string) (*RepoMap, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("repomap: %s is not a directory", root)
	}

	files, err := Walk(absRoot, s.Walk)
	if err != nil {
		return nil, fmt.Errorf("repomap: walk: %w", err)
	}

	slug := s.Slug
	if slug == "" {
		slug = Slugify(filepath.Base(absRoot))
	}

	rm := &RepoMap{
		SchemaVersion: SchemaVersion,
		Root:          absRoot,
		Slug:          slug,
		GeneratedAt:   s.now(),
		Languages:     map[string]LangStat{},
	}

	if meta, err := ReadGitMeta(absRoot); err == nil {
		rm.Git = meta
	}

	for _, f := range files {
		if f.Lang == "" {
			continue
		}
		st := rm.Languages[f.Lang]
		st.Files++
		st.LOC += f.LOC
		rm.Languages[f.Lang] = st
	}

	manifests := s.readManifests(absRoot, files)
	rm.Modules = s.buildModules(absRoot, files, manifests)
	rm.EntryPoints = s.findEntryPoints(absRoot, files, manifests, rm.Modules)
	rm.Docs = s.collectDocs(absRoot, files)
	rm.Protos = s.collectProtos(absRoot, files)
	rm.BuildTargets = s.collectBuildTargets(absRoot, files)
	rm.Services = s.collectServices(absRoot, files)

	attachEntryPoints(rm)
	rm.Hash = rm.computeHash()
	return rm, nil
}

// manifest is a parsed module declaration found during the walk.
type manifest struct {
	dir  string
	kind string
	path string

	goMod *GoModule
	cargo *CargoManifest
	node  *NodeManifest
}

func (s *Scanner) readManifests(root string, files []FileRec) []manifest {
	var out []manifest

	for _, f := range files {
		dir := path.Dir(f.Path)
		if dir == "." {
			dir = "."
		}
		base := path.Base(f.Path)

		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}

		switch base {
		case "go.mod":
			if m, err := ParseGoMod(dir, body); err == nil {
				out = append(out, manifest{dir: dir, kind: "go", path: f.Path, goMod: m})
			}
		case "Cargo.toml":
			if m, err := ParseCargoToml(body); err == nil && !m.IsWorkspaceOnly {
				out = append(out, manifest{dir: dir, kind: "rust", path: f.Path, cargo: m})
			}
		case "package.json":
			if m, err := ParsePackageJSON(body); err == nil && m.Name != "" {
				out = append(out, manifest{dir: dir, kind: "node", path: f.Path, node: m})
			}
		case "pyproject.toml":
			out = append(out, manifest{dir: dir, kind: "python", path: f.Path})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

func (s *Scanner) buildModules(root string, files []FileRec, manifests []manifest) []Module {
	dirs := make([]string, 0, len(manifests))
	byDir := make(map[string]manifest, len(manifests))
	for _, m := range manifests {
		dirs = append(dirs, m.dir)
		byDir[m.dir] = m
	}

	// Attribute every file to its owning manifest by longest prefix, so a file
	// under apps/ripple belongs there rather than to the root module.
	owned := map[string][]FileRec{}
	for _, f := range files {
		if dir := owningDir(dirs, f.Path); dir != "" {
			owned[dir] = append(owned[dir], f)
		}
	}

	taken := map[string]bool{}
	var out []Module

	for _, m := range manifests {
		mod := s.buildModule(root, m, owned[m.dir], taken)
		parts := s.subPartition(mod, owned[m.dir], taken)

		out = append(out, mod)
		out = append(out, parts...)
	}

	// A directory with source files but no manifest at all still deserves a
	// page; without this, sawmill's migrations/ and formats.d/ would vanish.
	if len(manifests) == 0 && len(files) > 0 {
		out = append(out, s.buildModule(root, manifest{dir: ".", kind: "generic"}, files, taken))
	}

	resolveEdges(out, manifests)
	sortModules(out)
	return out
}

// sortModules orders parents alphabetically with their sub-partitions directly
// beneath them. Sorting by slug alone puts `cmd-auth` before `watchtower`, so
// a repository's parts appear before the thing they are parts of.
func sortModules(mods []Module) {
	sortKey := func(m Module) string {
		if m.Parent == "" {
			return m.Slug + "\x00"
		}
		return m.Parent + "\x01" + m.Slug
	}
	sort.Slice(mods, func(i, j int) bool { return sortKey(mods[i]) < sortKey(mods[j]) })
}

func (s *Scanner) buildModule(root string, m manifest, files []FileRec, taken map[string]bool) Module {
	mod := Module{
		Dir:      m.dir,
		Kind:     m.kind,
		Manifest: m.path,
	}
	if mod.Kind == "" {
		mod.Kind = "generic"
	}

	switch {
	case m.goMod != nil:
		mod.Name = m.goMod.Path
		mod.ExternalDeps = m.goMod.Requires
		mod.Packages = goPackageDirs(m.dir, pathsOf(files))
	case m.cargo != nil:
		mod.Name = m.cargo.Name
		mod.ExternalDeps = m.cargo.Deps
		mod.Packages = rustModPaths(m.dir, pathsOf(files))
	case m.node != nil:
		mod.Name = m.node.Name
		mod.ExternalDeps = m.node.Deps
	}
	if mod.Name == "" {
		if m.dir == "." {
			mod.Name = filepath.Base(root)
		} else {
			mod.Name = m.dir
		}
	}

	// A root module gets a fixed slug. Deriving it from the directory name
	// would tie the module hash to a per-run checkout path, so every run would
	// see a change and nothing would ever be skipped.
	base := Slugify(m.dir)
	if m.dir == "." {
		base = RootModuleSlug
	}
	mod.Slug = uniqueSlug(taken, base)

	for _, f := range files {
		mod.FileCount++
		mod.LOC += f.LOC
		mod.Files = append(mod.Files, f.Path)
		if f.IsDoc {
			mod.Docs = append(mod.Docs, f.Path)
		}
		if f.Lang != "" && !containsString(mod.Languages, f.Lang) {
			mod.Languages = append(mod.Languages, f.Lang)
		}
	}
	sort.Strings(mod.Languages)

	mod.Empty = mod.FileCount == 0
	mod.Hash = hashFiles(files)
	return mod
}

// subPartition splits a large single-manifest module by its top-level package
// directories. Each part becomes its own page.
func (s *Scanner) subPartition(parent Module, files []FileRec, taken map[string]bool) []Module {
	if parent.LOC < SubPartitionMinLOC {
		return nil
	}

	groups := map[string][]FileRec{}
	for _, f := range files {
		rel := relativeTo(parent.Dir, f.Path)
		if rel == "" || rel == "." {
			continue
		}
		parts := strings.Split(rel, "/")
		if len(parts) < 2 {
			continue
		}
		if !containsString(subPartitionDirs, parts[0]) {
			continue
		}
		// Group one level below the convention dir: internal/auth, cmd/gateway.
		key := parts[0]
		if len(parts) >= 3 {
			key = parts[0] + "/" + parts[1]
		}
		groups[key] = append(groups[key], f)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []Module
	for _, k := range keys {
		group := groups[k]
		if len(group) == 0 {
			continue
		}
		dir := path.Join(parent.Dir, k)
		part := Module{
			Name:      k,
			Dir:       dir,
			Kind:      parent.Kind,
			Parent:    parent.Slug,
			Slug:      uniqueSlug(taken, Slugify(dir)),
			FileCount: len(group),
			Hash:      hashFiles(group),
		}
		for _, f := range group {
			part.LOC += f.LOC
			part.Files = append(part.Files, f.Path)
			if f.Lang != "" && !containsString(part.Languages, f.Lang) {
				part.Languages = append(part.Languages, f.Lang)
			}
		}
		sort.Strings(part.Languages)
		out = append(out, part)
	}
	return out
}

// resolveEdges converts manifest dependency declarations into intra-repo edges.
func resolveEdges(modules []Module, manifests []manifest) {
	byDir := map[string]*Module{}
	byName := map[string]*Module{}
	for i := range modules {
		byDir[modules[i].Dir] = &modules[i]
		byName[modules[i].Name] = &modules[i]
	}

	for _, m := range manifests {
		src := byDir[m.dir]
		if src == nil {
			continue
		}

		var localPaths []string
		switch {
		case m.goMod != nil:
			localPaths = m.goMod.LocalReplaces
		case m.cargo != nil:
			localPaths = m.cargo.LocalDeps
		}

		for _, lp := range localPaths {
			target := path.Clean(path.Join(m.dir, lp))
			if dep := byDir[target]; dep != nil && dep.Slug != src.Slug {
				src.DependsOn = append(src.DependsOn, dep.Slug)
			}
		}

		// A Go module requiring a sibling module's path is also an edge, even
		// without a replace directive.
		if m.goMod != nil {
			for _, req := range m.goMod.Requires {
				if dep := byName[req]; dep != nil && dep.Slug != src.Slug {
					src.DependsOn = append(src.DependsOn, dep.Slug)
				}
			}
		}
		sort.Strings(src.DependsOn)
		src.DependsOn = dedupeSorted(src.DependsOn)
	}
}

func (s *Scanner) findEntryPoints(root string, files []FileRec, manifests []manifest, modules []Module) []EntryPoint {
	var out []EntryPoint

	dirs := make([]string, 0, len(modules))
	slugByDir := map[string]string{}
	for _, m := range modules {
		if m.Parent == "" {
			dirs = append(dirs, m.Dir)
			slugByDir[m.Dir] = m.Slug
		}
	}
	moduleFor := func(p string) string {
		return slugByDir[owningDir(dirs, p)]
	}

	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".go") || f.Hash == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil || !hasGoMain(string(body)) {
			continue
		}
		out = append(out, EntryPoint{
			Path: f.Path, Module: moduleFor(f.Path),
			Kind: "go-main", Name: path.Base(path.Dir(f.Path)),
		})
	}

	for _, m := range manifests {
		switch {
		case m.cargo != nil:
			for _, b := range m.cargo.Bins {
				p := b.Path
				if p == "" {
					p = "src/main.rs"
				}
				out = append(out, EntryPoint{
					Path: path.Join(m.dir, p), Module: moduleFor(m.dir),
					Kind: "cargo-bin", Name: b.Name,
				})
			}
		case m.node != nil:
			for _, b := range m.node.Bins {
				out = append(out, EntryPoint{
					Path: m.path, Module: moduleFor(m.dir),
					Kind: "npm-bin", Name: b,
				})
			}
		}
	}

	for _, f := range files {
		base := path.Base(f.Path)
		if base == "Dockerfile" {
			out = append(out, EntryPoint{
				Path: f.Path, Module: moduleFor(f.Path),
				Kind: "dockerfile", Name: path.Dir(f.Path),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// attachEntryPoints records each module's entry points on the module itself.
func attachEntryPoints(rm *RepoMap) {
	byslug := map[string]*Module{}
	for i := range rm.Modules {
		byslug[rm.Modules[i].Slug] = &rm.Modules[i]
	}
	for _, ep := range rm.EntryPoints {
		if m := byslug[ep.Module]; m != nil {
			m.EntryPoints = append(m.EntryPoints, ep.Path)
		}
	}
}

func (s *Scanner) collectDocs(root string, files []FileRec) []DocFile {
	var out []DocFile
	for _, f := range files {
		if !f.IsDoc {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		out = append(out, DocFile{
			Path:  f.Path,
			Title: DocTitle(f.Path, body),
			Kind:  DocKind(f.Path),
			Hash:  f.Hash,
			Bytes: f.Size,
		})
	}
	return out
}

func (s *Scanner) collectProtos(root string, files []FileRec) []ProtoFile {
	var out []ProtoFile
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".proto") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		out = append(out, ParseProto(f.Path, body))
	}
	return out
}

func (s *Scanner) collectBuildTargets(root string, files []FileRec) []BuildTarget {
	var out []BuildTarget
	for _, f := range files {
		base := path.Base(f.Path)
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		switch base {
		case "Makefile", "makefile", "GNUmakefile":
			out = append(out, ParseMakefile(f.Path, body)...)
		case "Procfile":
			out = append(out, ParseProcfile(f.Path, body)...)
		}
	}
	return out
}

func (s *Scanner) collectServices(root string, files []FileRec) []ComposeService {
	var out []ComposeService
	for _, f := range files {
		base := path.Base(f.Path)
		if !strings.HasPrefix(base, "docker-compose") ||
			(!strings.HasSuffix(base, ".yml") && !strings.HasSuffix(base, ".yaml")) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		out = append(out, ParseCompose(body)...)
	}
	return out
}

// ToWorkspaceMap converts a RepoMap into the pipeline's partitioning.
//
// Empty modules are omitted: they are recorded in the map so it stays faithful,
// but sending a directory with no files to an LLM wastes money on nothing.
func (rm *RepoMap) ToWorkspaceMap() *mapper.WorkspaceMap {
	wm := &mapper.WorkspaceMap{
		SchemaVersion: SchemaVersion,
		Kind:          "git",
		Root:          rm.Root,
		GeneratedAt:   rm.GeneratedAt,
		Hash:          rm.Hash,
		Summary:       rm.Render(),
	}

	for _, m := range rm.Modules {
		if m.Empty {
			continue
		}
		wm.Units = append(wm.Units, mapper.Unit{
			Key:    "module:" + m.Slug,
			Kind:   "module",
			Slug:   m.Slug,
			Title:  m.Name,
			Dir:    m.Dir,
			Inputs: m.entryInputs(),
			Hash:   m.Hash,
			LOC:    m.LOC,
			Meta: map[string]any{
				"kind":      m.Kind,
				"languages": m.Languages,
				"parent":    m.Parent,
			},
		})
		for _, dep := range m.DependsOn {
			wm.Edges = append(wm.Edges, mapper.Edge{
				From: "module:" + m.Slug, To: "module:" + dep, Kind: "requires",
			})
		}
	}

	for _, d := range rm.Docs {
		wm.Units = append(wm.Units, mapper.Unit{
			Key: "doc:" + d.Path, Kind: "doc",
			Slug:  Slugify(strings.TrimSuffix(d.Path, path.Ext(d.Path))),
			Title: d.Title, Inputs: []string{d.Path}, Hash: d.Hash,
			Meta: map[string]any{"kind": d.Kind},
		})
	}

	return wm
}

// entryInputs picks the paths worth pointing the agent at first: entry points
// and docs, falling back to the module directory.
func (m Module) entryInputs() []string {
	var out []string
	out = append(out, m.EntryPoints...)
	out = append(out, m.Docs...)
	if m.Manifest != "" {
		out = append(out, m.Manifest)
	}
	if len(out) == 0 {
		out = append(out, m.Dir)
	}
	return out
}

// computeHash digests the structural shape of the map. A change here dirties
// the architecture synthesis pages.
func (rm *RepoMap) computeHash() string {
	h := sha256.New()
	for _, m := range rm.Modules {
		fmt.Fprintf(h, "%s|%s|%s|%d|%s\n", m.Slug, m.Dir, m.Kind, m.LOC, strings.Join(m.DependsOn, ","))
	}
	for _, e := range rm.EntryPoints {
		fmt.Fprintf(h, "ep|%s|%s|%s\n", e.Path, e.Kind, e.Name)
	}
	for _, p := range rm.Protos {
		fmt.Fprintf(h, "proto|%s|%s\n", p.Path, strings.Join(p.Services, ","))
	}
	for _, svc := range rm.Services {
		fmt.Fprintf(h, "svc|%s|%s\n", svc.Name, svc.Image)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashFiles(files []FileRec) string {
	sorted := make([]FileRec, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s|%s\n", f.Path, f.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func pathsOf(files []FileRec) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func (s *Scanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
