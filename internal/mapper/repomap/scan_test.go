package repomap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture writes a tree from a path->content map and returns its root.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()

	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func scanFixture(t *testing.T, files map[string]string) *RepoMap {
	t.Helper()
	s := &Scanner{Now: func() time.Time { return time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC) }}

	rm, err := s.Scan(context.Background(), fixture(t, files))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return rm
}

func moduleBySlug(rm *RepoMap, slug string) *Module {
	for i := range rm.Modules {
		if rm.Modules[i].Slug == slug {
			return &rm.Modules[i]
		}
	}
	return nil
}

func TestScanSiblingModulesWithLocalReplace(t *testing.T) {
	// The InfraFlux shape: sibling modules, no root manifest, wired together
	// with replace directives.
	rm := scanFixture(t, map[string]string{
		"shared/go.mod":     "module github.com/x/shared\n\ngo 1.24\n",
		"shared/shared.go":  "package shared\n\nfunc Helper() {}\n",
		"pilot/go.mod":      "module github.com/x/pilot\n\ngo 1.24\n\nrequire github.com/x/shared v0.0.0\n\nreplace github.com/x/shared => ../shared\n",
		"pilot/cmd/main.go": "package main\n\nfunc main() {}\n",
	})

	if len(rm.Modules) != 2 {
		t.Fatalf("got %d modules, want 2: %+v", len(rm.Modules), rm.Modules)
	}

	pilot := moduleBySlug(rm, "pilot")
	if pilot == nil {
		t.Fatal("pilot module not found")
	}
	// The replace directive is a real intra-repo edge, not an external dep.
	if len(pilot.DependsOn) != 1 || pilot.DependsOn[0] != "shared" {
		t.Errorf("pilot.DependsOn = %v, want [shared]", pilot.DependsOn)
	}
}

func TestScanNonGitDirectory(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	// Not being a repository is a supported state, not a failure: change
	// detection falls back to content hashes.
	if rm.Git != nil {
		t.Errorf("Git = %+v, want nil for a non-repository", rm.Git)
	}
	if !strings.Contains(rm.Render(), "Not a git repository") {
		t.Error("render should say the directory is not a repository")
	}
}

func TestScanSubPartitionsLargeModule(t *testing.T) {
	// The watchtower shape: one manifest, many services. Build a module large
	// enough to cross the split threshold.
	files := map[string]string{"go.mod": "module example.com/big\n\ngo 1.24\n"}
	// Each file must push the module past SubPartitionMinLOC in aggregate.
	body := strings.Repeat("// filler line\n", SubPartitionMinLOC/2)
	for _, dir := range []string{"internal/auth", "internal/cluster", "cmd/gateway", "pkg/config"} {
		files[dir+"/file.go"] = "package x\n" + body
	}

	rm := scanFixture(t, files)

	for _, want := range []string{"internal-auth", "internal-cluster", "cmd-gateway", "pkg-config"} {
		m := moduleBySlug(rm, want)
		if m == nil {
			t.Errorf("sub-partition %q missing; a large repo would collapse into one page", want)
			continue
		}
		if m.Parent == "" {
			t.Errorf("%q has no parent recorded", want)
		}
	}
}

func TestScanDoesNotSubPartitionSmallModule(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":                "module example.com/small\n\ngo 1.24\n",
		"internal/auth/auth.go": "package auth\n\nfunc F() {}\n",
	})

	for _, m := range rm.Modules {
		if m.Parent != "" {
			t.Errorf("small module was split into %q; splitting below the threshold adds noise", m.Slug)
		}
	}
}

func TestScanOrdersParentsBeforeChildren(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/z-last\n\ngo 1.24\n"}
	body := strings.Repeat("// filler\n", SubPartitionMinLOC)
	for _, dir := range []string{"cmd/a", "internal/b"} {
		files[dir+"/f.go"] = "package x\n" + body
	}

	rm := scanFixture(t, files)

	// Sorting by slug alone would put cmd-a before its parent, so a
	// repository's parts appear before the thing they are parts of.
	var seenParent bool
	for _, m := range rm.Modules {
		if m.Parent == "" {
			seenParent = true
			continue
		}
		if !seenParent {
			t.Fatalf("sub-partition %q appeared before its parent", m.Slug)
		}
	}
}

func TestScanFindsEntryPoints(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":             "module example.com/x\n\ngo 1.24\n",
		"cmd/server/main.go": "package main\n\nfunc main() {}\n",
		"internal/lib.go":    "package internal\n",
		"Dockerfile":         "FROM golang:1.24\nCMD [\"/app\"]\n",
	})

	var kinds []string
	for _, ep := range rm.EntryPoints {
		kinds = append(kinds, ep.Kind)
	}
	if !containsString(kinds, "go-main") {
		t.Errorf("no go-main entry point found: %+v", rm.EntryPoints)
	}
	if !containsString(kinds, "dockerfile") {
		t.Errorf("no dockerfile entry point found: %+v", rm.EntryPoints)
	}
	// A non-main package must not be mistaken for an entry point.
	for _, ep := range rm.EntryPoints {
		if strings.Contains(ep.Path, "internal/lib.go") {
			t.Error("a non-main package was recorded as an entry point")
		}
	}
}

func TestScanExcludesSecretsAndBuildOutput(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":                  "module example.com/x\n\ngo 1.24\n",
		"main.go":                 "package main\n\nfunc main() {}\n",
		".env":                    "DATABASE_PASSWORD=hunter2\n",
		"secrets.pem":             "-----BEGIN PRIVATE KEY-----\n",
		"node_modules/pkg/idx.js": "module.exports = {}\n",
		"target/debug/build.log":  "compiling\n",
		"go.sum":                  "h1:abc\n",
	})

	all := strings.Join(allFilePaths(rm), " ")
	// A credential that reaches the wiki has been published to everyone with
	// access to the workspace.
	for _, forbidden := range []string{".env", "secrets.pem", "node_modules", "target/", "go.sum"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("%q was indexed; it must be excluded", forbidden)
		}
	}
	if !strings.Contains(all, "main.go") {
		t.Error("main.go was not indexed")
	}
}

func allFilePaths(rm *RepoMap) []string {
	var out []string
	for _, m := range rm.Modules {
		out = append(out, m.Files...)
	}
	return out
}

func TestScanCollectsArtifacts(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":             "module example.com/x\n\ngo 1.24\n",
		"main.go":            "package main\n\nfunc main() {}\n",
		"proto/api.proto":    "syntax = \"proto3\";\npackage api.v1;\n\nservice ApiService {\n  rpc Get(Req) returns (Res);\n}\n\nmessage Req {}\n",
		"Makefile":           "build:\n\tgo build\n\ntest:\n\tgo test\n\nVAR := notatarget\n",
		"Procfile":           "web: ./server\nworker: ./worker\n",
		"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n    ports:\n      - \"5432:5432\"\n  api:\n    build: .\n    depends_on:\n      - db\n",
		"README.md":          "# My Project\n\nDescription.\n",
	})

	if len(rm.Protos) != 1 || rm.Protos[0].Package != "api.v1" {
		t.Errorf("Protos = %+v", rm.Protos)
	}
	if len(rm.Protos) > 0 && !containsString(rm.Protos[0].Services, "ApiService") {
		t.Errorf("proto services = %v", rm.Protos[0].Services)
	}

	var makeNames, procNames []string
	for _, tgt := range rm.BuildTargets {
		if strings.HasSuffix(tgt.File, "Makefile") {
			makeNames = append(makeNames, tgt.Name)
		} else {
			procNames = append(procNames, tgt.Name)
		}
	}
	if !containsString(makeNames, "build") || !containsString(makeNames, "test") {
		t.Errorf("Makefile targets = %v", makeNames)
	}
	// A variable assignment is not a target.
	if containsString(makeNames, "VAR") {
		t.Errorf("variable assignment parsed as a target: %v", makeNames)
	}
	if !containsString(procNames, "web") {
		t.Errorf("Procfile processes = %v", procNames)
	}

	if len(rm.Services) != 2 {
		t.Fatalf("Services = %+v, want 2", rm.Services)
	}
	if len(rm.Docs) != 1 || rm.Docs[0].Title != "My Project" {
		t.Errorf("Docs = %+v", rm.Docs)
	}
}

func TestScanComposeNeverCapturesEnvValues(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":             "module example.com/x\n\ngo 1.24\n",
		"main.go":            "package main\n\nfunc main() {}\n",
		"docker-compose.yml": "services:\n  db:\n    image: postgres:16\n    environment:\n      - POSTGRES_PASSWORD=hunter2\n      - POSTGRES_USER=admin\n",
	})

	rendered := rm.Render()
	// Compose files routinely hold credentials, and a wiki is readable by
	// everyone with workspace access.
	if strings.Contains(rendered, "hunter2") {
		t.Error("an environment value leaked into the rendered map")
	}
	for _, svc := range rm.Services {
		for _, k := range svc.EnvKeys {
			if strings.Contains(k, "hunter2") {
				t.Errorf("EnvKeys captured a value: %v", svc.EnvKeys)
			}
		}
	}
}

func TestScanHashIsStableAndSensitive(t *testing.T) {
	base := map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}

	first := scanFixture(t, base)
	second := scanFixture(t, base)
	// A stable hash across identical inputs is what makes a no-op run free.
	if first.Hash != second.Hash {
		t.Error("map hash differs across scans of identical trees")
	}

	changed := map[string]string{
		"go.mod":            base["go.mod"],
		"main.go":           base["main.go"],
		"cmd/other/main.go": "package main\n\nfunc main() {}\n",
	}
	if third := scanFixture(t, changed); third.Hash == first.Hash {
		t.Error("map hash did not change when a new entry point appeared")
	}
}

func TestScanHashIsIndependentOfCheckoutPath(t *testing.T) {
	// kiln scans a per-run checkout directory whose name differs every time.
	// If anything path-derived reached the hash, every run would look changed,
	// nothing would ever be skipped, and the whole cost model would collapse.
	files := map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}

	a := scanFixture(t, files)
	b := scanFixture(t, files)

	if a.Root == b.Root {
		t.Fatal("fixture roots coincided; the test cannot detect path leakage")
	}
	if a.Hash != b.Hash {
		t.Errorf("map hash depends on the checkout path:\n  %s\n  %s", a.Hash, b.Hash)
	}
	if a.Modules[0].Hash != b.Modules[0].Hash {
		t.Error("module hash depends on the checkout path")
	}
	if got := a.Modules[0].Slug; got != RootModuleSlug {
		t.Errorf("root module slug = %q, want %q; a path-derived slug varies per run", got, RootModuleSlug)
	}
}

func TestScanModuleHashTracksContent(t *testing.T) {
	before := scanFixture(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	after := scanFixture(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() { println(\"changed\") }\n",
	})

	// The module hash is the gate that decides whether an LLM call happens.
	if before.Modules[0].Hash == after.Modules[0].Hash {
		t.Error("module hash unchanged after its content changed")
	}
}

func TestToWorkspaceMapSkipsEmptyModules(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	// Force an empty module, as moneypal/butler is on disk.
	rm.Modules = append(rm.Modules, Module{Slug: "butler", Name: "butler", Empty: true})

	wm := rm.ToWorkspaceMap()
	for _, u := range wm.Units {
		if u.Slug == "butler" {
			t.Error("an empty module became a unit; sending it to an LLM spends money on nothing")
		}
	}
}

func TestToWorkspaceMapCarriesEdges(t *testing.T) {
	rm := scanFixture(t, map[string]string{
		"shared/go.mod":    "module github.com/x/shared\n\ngo 1.24\n",
		"shared/shared.go": "package shared\n",
		"pilot/go.mod":     "module github.com/x/pilot\n\ngo 1.24\n\nrequire github.com/x/shared v0.0.0\n\nreplace github.com/x/shared => ../shared\n",
		"pilot/pilot.go":   "package pilot\n",
	})

	wm := rm.ToWorkspaceMap()
	var found bool
	for _, e := range wm.Edges {
		if e.From == "module:pilot" && e.To == "module:shared" {
			found = true
		}
	}
	if !found {
		t.Errorf("dependency edge missing from the workspace map: %+v", wm.Edges)
	}
}

func TestScanEmptyDirectory(t *testing.T) {
	s := &Scanner{}
	rm, err := s.Scan(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Scan of an empty directory: %v", err)
	}
	if len(rm.Modules) != 0 {
		t.Errorf("Modules = %+v, want none", rm.Modules)
	}
}

func TestScanRejectsNonDirectory(t *testing.T) {
	root := fixture(t, map[string]string{"file.txt": "x"})

	s := &Scanner{}
	if _, err := s.Scan(context.Background(), filepath.Join(root, "file.txt")); err == nil {
		t.Error("Scan accepted a file path")
	}
}
