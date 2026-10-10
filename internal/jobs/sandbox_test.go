package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/mapper"
)

// ripple is a unit with a directory and a real input, which is what makes the
// source guard look at more than the root.
func rippleUnit() mapper.Unit {
	return mapper.Unit{
		Key: "module:ripple", Slug: "ripple", Hash: "h", Kind: "module",
		Dir: "apps/ripple", Inputs: []string{"apps/ripple/main.go"},
	}
}

// rippleRequest builds only the module's unit (a change under its directory),
// so what is imported can be attributed to it: a full rebuild also plans the
// architecture unit, which would write the same scripted page.
func rippleRequest(t *testing.T) BuildRequest {
	t.Helper()
	return testRequest(t, testMap(rippleUnit()), diff.ChangeSet{Changes: []diff.Change{
		{Path: "apps/ripple/main.go", Kind: diff.Modified},
	}})
}

func writeRippleSources(t *testing.T, root string) {
	t.Helper()
	writeTree(t, root, map[string]string{
		"README.md":                 "# repo\n",
		"apps/ripple/main.go":       "package main\n",
		"apps/ripple/util/util.go":  "package util\n",
		"apps/other/other.go":       "package other\n",
		"apps/other/deep/deeper.go": "package deeper\n",
	})
}

// scriptedCLI is the real ClaudeRunner driving the fakeclaude fixture.
func scriptedCLI(t *testing.T, script map[string]any) *agent.ClaudeRunner {
	t.Helper()
	b, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	return &agent.ClaudeRunner{
		Binary: fakeClaudeBinary(t),
		Env:    append(os.Environ(), "KILN_FAKE_SCRIPT="+string(b)),
	}
}

func itemFor(t *testing.T, items []ItemSummary, key string) ItemSummary {
	t.Helper()
	for _, it := range items {
		if string(it.Key) == key {
			return it
		}
	}
	t.Fatalf("no item for %s in %+v", key, items)
	return ItemSummary{}
}

// The end-to-end half of the sandbox. An agent that gets past the CLI's own
// refusal and writes into the sources must fail its unit, and the pages it
// wrote must not be imported -- however well-formed they are, and they are here.
func TestCLIUnitThatWritesTheSourcesFailsAndImportsNothing(t *testing.T) {
	cases := map[string]struct {
		file    string // relative to the source root
		content string
	}{
		"a stray file beside the sources": {"NOTES.md", "injected\n"},
		"an edit to the input it read":    {"apps/ripple/main.go", "package main\n\n// injected\n"},
		"a new file in the unit's tree":   {"apps/ripple/util/new.go", "package util\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			req := rippleRequest(t)
			writeRippleSources(t, req.SourceDir)

			// On the generate step only: a valid page into the working directory
			// (the scratch dir), and a write into the sources beside it.
			runner := scriptedCLI(t, map[string]any{
				"write_files":     map[string]string{"entities/ripple.md": validPage("entity", "Ripple")},
				"write_files_abs": map[string]string{filepath.Join(req.SourceDir, tc.file): tc.content},
				"write_on":        "generate",
				"envelope": map[string]any{
					"subtype": "success", "terminal_reason": "completed",
					"total_cost_usd": 0.01, "num_turns": 1,
				},
			})

			p := testPipeline(store, runner)
			res, err := p.Build(context.Background(), req)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			item := itemFor(t, res.Summary.Items, "module:ripple")
			if item.Status != StatusFailed {
				t.Errorf("module:ripple status = %q, want failed", item.Status)
			}
			if !strings.Contains(item.Err, errSourcesModified.Error()) || !strings.Contains(item.Err, filepath.Base(tc.file)) {
				t.Errorf("module:ripple error = %q, want the tamper error naming %s", item.Err, tc.file)
			}
			if !strings.Contains(item.Err, "nothing was restored") {
				t.Errorf("the error does not say the files were left as they are: %q", item.Err)
			}

			// The page the agent wrote beside the tampering is not imported.
			if imp := store.lastImport(); imp != nil {
				for _, pg := range imp.UpsertPages {
					if pg.Path == "entities/ripple.md" {
						t.Errorf("a unit that modified the sources had its page imported: %+v", imp.UpsertPages)
					}
				}
			}

			// And nothing was put back: the evidence is the operator's to see.
			got, err := os.ReadFile(filepath.Join(req.SourceDir, tc.file))
			if err != nil || string(got) != tc.content {
				t.Errorf("the guard touched the modified file: %q, %v", got, err)
			}
		})
	}
}

// The other half: an agent that writes only where it is allowed still imports,
// so the guard costs a well-behaved build nothing but the look.
func TestCLIUnitThatWritesOnlyTheScratchDirStillImports(t *testing.T) {
	store := newMemStore()
	req := rippleRequest(t)
	writeRippleSources(t, req.SourceDir)
	before := snapshotSources(req.SourceDir, rippleUnit())

	runner := scriptedCLI(t, map[string]any{
		"write_files": map[string]string{"entities/ripple.md": validPage("entity", "Ripple")},
		"envelope": map[string]any{
			"subtype": "success", "terminal_reason": "completed",
			"total_cost_usd": 0.01, "num_turns": 1,
		},
	})
	res, err := testPipeline(store, runner).Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if item := itemFor(t, res.Summary.Items, "module:ripple"); item.Status != StatusSucceeded {
		t.Fatalf("module:ripple status = %q (%s), want succeeded", item.Status, item.Err)
	}
	imp := store.lastImport()
	if imp == nil {
		t.Fatal("nothing was imported")
	}
	var found bool
	for _, pg := range imp.UpsertPages {
		found = found || pg.Path == "entities/ripple.md"
	}
	if !found {
		t.Errorf("the page written to the scratch dir never reached the import: %+v", imp.UpsertPages)
	}
	if changes := before.changesTo(snapshotSources(req.SourceDir, rippleUnit())); len(changes) != 0 {
		t.Errorf("a well-behaved build changed the sources: %v", changes)
	}
}

// A tamper during analyze is caught before generation is paid for.
func TestSourcesModifiedDuringAnalyzeStopsTheUnitBeforeGenerate(t *testing.T) {
	store := newMemStore()
	req := rippleRequest(t)
	writeRippleSources(t, req.SourceDir)
	argvLog := filepath.Join(t.TempDir(), "argv.jsonl")

	runner := scriptedCLI(t, map[string]any{
		"write_files_abs": map[string]string{filepath.Join(req.SourceDir, "NOTES.md"): "x"},
		"write_on":        "analyze",
		"append_args_to":  argvLog,
		"envelope": map[string]any{
			"subtype": "success", "terminal_reason": "completed",
			"total_cost_usd": 0.01, "num_turns": 1,
		},
	})
	res, err := testPipeline(store, runner).Build(context.Background(), req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	item := itemFor(t, res.Summary.Items, "module:ripple")
	if item.Status != StatusFailed || !strings.Contains(item.Err, errSourcesModified.Error()) {
		t.Errorf("module:ripple = %q / %q, want a tamper failure", item.Status, item.Err)
	}
	// Only analyze was ever run for the module: generation was not paid for.
	for _, c := range readCLICalls(t, argvLog) {
		if flagValue(c.Args, "--resume") == agent.SessionUUID("run-1-"+sanitize("module:ripple")) {
			t.Errorf("generate ran for a unit whose sources were modified during analyze")
		}
	}
}

// What the snapshot sees and, as deliberately, what it does not.
func TestSourceSnapshotDetectsChangesWhereTheAgentWorks(t *testing.T) {
	unit := rippleUnit()
	edit := func(root, rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		mutate func(root string)
		want   string // substring of the changes; "" means none expected
	}{
		{"untouched", func(string) {}, ""},
		{"input edited", func(r string) { edit(r, "apps/ripple/main.go", "package main // changed\n") }, "modified apps/ripple/main.go"},
		{"input touched", func(r string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(r, "apps/ripple/main.go"), later, later); err != nil {
				t.Fatal(err)
			}
		}, "modified apps/ripple/main.go"},
		{"file added at the root", func(r string) { edit(r, "pages.md", "x") }, "added pages.md"},
		{"file added beside the input", func(r string) { edit(r, "apps/ripple/extra.go", "x") }, "added apps/ripple/extra.go"},
		{"file added deep in the unit", func(r string) { edit(r, "apps/ripple/util/sub/new.go", "x") }, "added apps/ripple/util/sub"},
		{"file removed from the unit", func(r string) {
			if err := os.Remove(filepath.Join(r, "apps/ripple/util/util.go")); err != nil {
				t.Fatal(err)
			}
		}, "removed apps/ripple/util/util.go"},
		{"os litter is ignored", func(r string) { edit(r, "apps/ripple/.DS_Store", "x") }, ""},
		// The accepted gap, pinned so it stays a decision rather than a surprise:
		// a deep file in an unrelated part of the tree is outside the bounded
		// look. The CLI's edit deny and --restricted are what stop that write.
		{"unrelated deep file edited", func(r string) { edit(r, "apps/other/deep/deeper.go", "package deeper // x\n") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeRippleSources(t, root)
			guard := newSourceGuard(root, unit)

			tc.mutate(root)

			changes, err := guard.verify()
			if tc.want == "" {
				if err != nil || len(changes) != 0 {
					t.Fatalf("reported %v (%v) for a change it should not see", changes, err)
				}
				return
			}
			if !errors.Is(err, errSourcesModified) {
				t.Fatalf("err = %v, want errSourcesModified (changes %v)", err, changes)
			}
			if !strings.Contains(strings.Join(changes, "\n"), tc.want) {
				t.Errorf("changes = %v, want one containing %q", changes, tc.want)
			}
			if !strings.Contains(err.Error(), root) {
				t.Errorf("the error does not say which tree: %v", err)
			}
		})
	}
}

func TestSourceGuardIsNilSafeAndSkipsRunnersWithNoCheckout(t *testing.T) {
	if g := newSourceGuard("", rippleUnit()); g != nil {
		t.Errorf("a unit with no root got a guard: %+v", g)
	}
	var g *sourceGuard
	if changes, err := g.verify(); err != nil || changes != nil {
		t.Errorf("nil guard verify = %v, %v", changes, err)
	}
}

// node_modules and .git are recorded but not entered: a file created directly
// in one still moves its directory, so it is seen, while its contents are not
// walked.
func TestSourceSnapshotDoesNotDescendIntoVendoredTrees(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"apps/ripple/main.go":                  "package main\n",
		"apps/ripple/node_modules/a/b/deep.js": "1",
	})
	unit := mapper.Unit{Key: "module:ripple", Dir: "apps/ripple", Inputs: []string{"apps/ripple/main.go"}}
	snap := snapshotSources(root, unit)

	if _, ok := snap.entries[filepath.Join("apps/ripple/node_modules")]; !ok {
		t.Error("node_modules itself is not recorded")
	}
	for rel := range snap.entries {
		if strings.HasPrefix(rel, filepath.Join("apps/ripple/node_modules")+string(filepath.Separator)) {
			t.Errorf("descended into node_modules: %s", rel)
		}
	}
}

// Past the cap the walk covers a different prefix of the tree each time it is
// taken, so an entry that fell off the end is not a removal. A real
// modification inside the covered part is still reported.
func TestSourceSnapshotCapDoesNotInventChanges(t *testing.T) {
	old := snapshotEntryCap
	snapshotEntryCap = 40
	t.Cleanup(func() { snapshotEntryCap = old })

	root := t.TempDir()
	files := map[string]string{"apps/ripple/main.go": "package main\n"}
	for i := range 100 {
		files[fmt.Sprintf("apps/ripple/f%03d.go", i)] = "package x\n"
	}
	writeTree(t, root, files)
	unit := mapper.Unit{Key: "module:ripple", Dir: "apps/ripple", Inputs: []string{"apps/ripple/main.go"}}

	guard := newSourceGuard(root, unit)
	if !guard.before.truncated {
		t.Fatal("the cap was not reached; the test proves nothing")
	}

	// A new file sorting before everything shifts which entries fit under the cap.
	writeTree(t, root, map[string]string{"apps/ripple/a-first.go": "package x\n"})
	changes, _ := guard.verify()
	for _, c := range changes {
		if strings.HasPrefix(c, "removed ") || strings.HasPrefix(c, "added apps/ripple/f") {
			t.Errorf("cap produced a phantom change: %q (all: %v)", c, changes)
		}
	}

	// And a real edit to the input, which is recorded flat, is never lost.
	writeTree(t, root, map[string]string{"apps/ripple/main.go": "package main // edited\n"})
	changes, err := guard.verify()
	if !errors.Is(err, errSourcesModified) || !strings.Contains(strings.Join(changes, "\n"), "modified apps/ripple/main.go") {
		t.Errorf("an edit to the input was missed under truncation: %v / %v", changes, err)
	}
}

// flagValues is every value a variadic flag takes: the arguments after it up to
// the next flag.
func flagValues(args []string, flag string) []string {
	for i, a := range args {
		if a != flag {
			continue
		}
		var vals []string
		for _, v := range args[i+1:] {
			if strings.HasPrefix(v, "--") {
				break
			}
			vals = append(vals, v)
		}
		return vals
	}
	return nil
}

// The scratch directory is now the CLI's working directory, so tool state can
// land in it. Pages are still found, and state in a dot directory is not one.
func TestCollectPagesIgnoresToolStateInTheWorkingDirectory(t *testing.T) {
	scratch := t.TempDir()
	writeTree(t, scratch, map[string]string{
		"entities/ripple.md":    validPage("entity", "Ripple"),
		".claude/notes.md":      "# not a page\n",
		".claude/sub/memory.md": "not a page either",
	})

	pages, violations, err := collectPages(scratch)
	if err != nil {
		t.Fatalf("collectPages: %v", err)
	}
	if len(violations) != 0 || len(pages) != 1 || pages[0].Path != "entities/ripple.md" {
		t.Errorf("pages = %v, violations = %v, want only entities/ripple.md", pages, violations)
	}
}

// Under the CLI's --restricted mode the agent's file tools reach only its
// working directory and the directories it is granted, so research has to be
// granted every root the selected units live in, not just the best match's.
func TestResearchReadDirsCoverEveryUnitRoot(t *testing.T) {
	units := []mapper.Unit{
		{Key: "doc:upload:a.pdf", Meta: map[string]any{mapper.MetaRoot: "/stage/docs"}},
		{Key: "module:x"}, // the repository
		{Key: "doc:upload:b.pdf", Meta: map[string]any{mapper.MetaRoot: "/stage/docs"}},
		{Key: "page:web:c", Meta: map[string]any{mapper.MetaRoot: "/stage/web"}},
	}
	workDir := researchWorkDir("/repo", units, nil)
	if workDir != "/stage/docs" {
		t.Fatalf("workDir = %q, want the best match's root", workDir)
	}

	got := researchReadDirs("/repo", units, workDir)
	want := []string{"/repo", "/stage/web"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("researchReadDirs = %v, want %v (no duplicates, never the working directory)", got, want)
	}

	if got := researchReadDirs("", nil, ""); len(got) != 0 {
		t.Errorf("a run with no repository and no units granted %v", got)
	}
}
