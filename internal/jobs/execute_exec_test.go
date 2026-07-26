package jobs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/diff"
)

// writeTree materializes a map of relative path -> content under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecuteDryRunSyncsMapsAndPlans(t *testing.T) {
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{
		"go.mod":  "module example.com/demo\n\ngo 1.25\n",
		"main.go": "package main\n\nfunc main() {}\n",
		"README.md": "# Demo\n\nA repository for the Execute test.\n" +
			strings.Repeat("Filler prose so the doc is not empty.\n", 5),
	})
	docs := t.TempDir()
	writeTree(t, docs, map[string]string{
		"notes.md": "# Notes\n\nUploaded document body.\n",
	})

	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())

	var progress bytes.Buffer
	res, err := p.Execute(context.Background(), ExecuteRequest{
		RunID:       "run-exec",
		WorkspaceID: "ws-1",
		Trigger:     "manual",
		Source:      SourceSpec{Path: repo, DocsDir: docs, Slug: "bench"},
		DryRun:      true,
		Progress:    &progress,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The dry run planned real units from both connectors and spent nothing.
	if len(res.Planned) == 0 {
		t.Fatal("dry run planned nothing")
	}
	var sawModule, sawUpload bool
	for _, k := range res.Planned {
		if k.Prefix() == "module" {
			sawModule = true
		}
		if k == diff.DocKey(diff.UploadOrigin("notes.md")) {
			sawUpload = true
		}
	}
	if !sawModule {
		t.Errorf("no module unit in plan: %v", res.Planned)
	}
	if !sawUpload {
		t.Errorf("uploaded doc missing from plan (namespaced key): %v", res.Planned)
	}
	if res.EstimatedUSD <= 0 {
		t.Errorf("estimate = %v, want > 0", res.EstimatedUSD)
	}
	if store.importCount() != 0 {
		t.Error("dry run imported")
	}

	// Progress narration names both connectors, like the CLI always printed.
	out := progress.String()
	for _, want := range []string{"syncing via git connector", "syncing via upload connector", "1 document(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress missing %q:\n%s", want, out)
		}
	}
}

func TestExecuteNilProgressAndMissingPathsFail(t *testing.T) {
	store := newMemStore()
	p := testPipeline(store, newScriptedRunner())
	ctx := context.Background()

	// A missing repo directory fails the sync, with no Progress required.
	_, err := p.Execute(ctx, ExecuteRequest{
		RunID: "r", WorkspaceID: "ws-1", Trigger: "manual",
		Source: SourceSpec{Path: filepath.Join(t.TempDir(), "gone"), Slug: "b"},
		DryRun: true,
	})
	if err == nil {
		t.Error("Execute on a missing repo path succeeded")
	}

	// A repo that syncs but a docs dir that does not still fails, after the
	// git half already ran.
	repo := t.TempDir()
	writeTree(t, repo, map[string]string{"main.go": "package main\nfunc main() {}\n"})
	_, err = p.Execute(ctx, ExecuteRequest{
		RunID: "r2", WorkspaceID: "ws-1", Trigger: "manual",
		Source: SourceSpec{Path: repo, DocsDir: filepath.Join(t.TempDir(), "gone"), Slug: "b"},
		DryRun: true,
	})
	if err == nil {
		t.Error("Execute with a missing docs dir succeeded")
	}
}

func TestNewPipelineWiresBudgetsAndModels(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Model = "claude-sonnet-5"
	cfg.Agent.AnalyzeModel = "claude-sonnet-5"
	cfg.Agent.FallbackModel = "claude-opus-5"
	cfg.Agent.AnalyzeBudgetUSD = 0.4
	cfg.Agent.PageBudgetUSD = 1.5
	cfg.Agent.RunBudgetUSD = 6
	cfg.Agent.MaxPagesPerRun = 12
	cfg.Agent.WarnTurns = 40

	st := newMemStore()
	runner := newScriptedRunner()
	p := NewPipeline(cfg, st, runner, nil)

	if p.Store != Store(st) || p.Runner != runner {
		t.Error("pipeline dependencies not wired")
	}
	if p.Budget.RunUSD != 6 || p.Budget.MaxPages != 12 || p.Budget.PageUSD != 1.5 {
		t.Errorf("budget wiring: %+v", p.Budget)
	}
	if p.Model != "claude-sonnet-5" || p.FallbackModel != "claude-opus-5" {
		t.Errorf("model wiring: %q / %q", p.Model, p.FallbackModel)
	}
	if p.MaxRetries != 1 {
		t.Errorf("MaxRetries = %d, want 1 (one corrective attempt)", p.MaxRetries)
	}
}
