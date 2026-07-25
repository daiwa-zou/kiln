package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/connector"
	gitconn "github.com/daiwa-zou/kiln/internal/connector/git"
	"github.com/daiwa-zou/kiln/internal/connector/upload"
	"github.com/daiwa-zou/kiln/internal/diff"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/mapper"
	"github.com/daiwa-zou/kiln/internal/mapper/docmap"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

type buildFlags struct {
	path      string
	docs      string
	workspace string
	org       string
	dryRun    bool
	full      bool
	budget    float64
	model     string
}

func newBuildCmd(g *globalFlags) *cobra.Command {
	var f buildFlags

	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Build or refresh a workspace's wiki from a local directory",
		Long: `Scans a directory, works out what changed since the last run, and regenerates
only the affected pages.

A run over unchanged sources makes no LLM calls and costs nothing, so repeating
a build is cheap by design. Use --dry-run to see the plan and a cost estimate
before anything is spent.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				f.path = args[0]
			}
			return runBuild(cmd, g, &f)
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&f.path, "path", "", "directory to scan (defaults to the positional argument)")
	fl.StringVar(&f.docs, "docs", "", "also ingest documents from this directory into the same wiki")
	fl.StringVar(&f.workspace, "workspace", "", "workspace slug (defaults to the directory name)")
	fl.StringVar(&f.org, "org", "local", "organization slug")
	fl.BoolVar(&f.dryRun, "dry-run", false, "plan and estimate without calling the model")
	fl.BoolVar(&f.full, "full", false, "ignore cached hashes and rebuild every unit")
	fl.Float64Var(&f.budget, "budget", 0, "override the per-run spend cap in USD")
	fl.StringVar(&f.model, "model", "", "override the model for this run")

	return cmd
}

func runBuild(cmd *cobra.Command, g *globalFlags, f *buildFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	if f.path == "" {
		return fmt.Errorf("a directory is required: kiln build <path>")
	}
	absPath, err := filepath.Abs(f.path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absPath); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", f.path)
	}

	slug := f.workspace
	if slug == "" {
		slug = repomap.Slugify(filepath.Base(absPath))
	}

	cfg, err := config.Load(config.Options{File: g.configFile, Role: config.RoleWorker})
	if err != nil {
		return err
	}
	if f.model != "" {
		cfg.Agent.Model = f.model
	}
	if f.budget > 0 {
		cfg.Agent.RunBudgetUSD = f.budget
	}

	log := observability.NewLogger(firstNonEmpty(g.logLevel, cfg.LogLevel))

	db, err := store.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.CheckSchemaVersion(ctx, db.Pool); err != nil {
		return err
	}

	js := store.NewJobStore(db.Pool)
	workspaceID, err := js.EnsureWorkspace(ctx, f.org, slug, filepath.Base(absPath))
	if err != nil {
		return err
	}

	// Sync goes through the connector registry rather than calling the scanner
	// directly, so the abstraction is exercised by the path that uses it rather
	// than assumed to work.
	conn, err := connector.Get("git")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "syncing via %s connector: %s\n", conn.Kind(), absPath)

	set, err := conn.Sync(ctx, connector.Config{"path": absPath, "slug": slug}, "")
	if err != nil {
		return err
	}

	// Routing and prompt grounding need the module graph, which a flat item
	// list cannot express. The git connector carried it on the same sync.
	rm := gitconn.MapOf(set)
	if rm == nil {
		return fmt.Errorf("git connector returned no repository map")
	}
	wm := rm.ToWorkspaceMap()
	fmt.Fprintf(out, "  %d modules, %d units, %d edges\n", len(rm.Modules), len(wm.Units), len(wm.Edges))
	if rm.Git != nil && rm.Git.HeadSHA != "" {
		fmt.Fprintf(out, "  at %s on %s\n", short(rm.Git.HeadSHA), rm.Git.Branch)
	} else {
		fmt.Fprintln(out, "  not a git repository; change detection uses content hashes")
	}

	router := routerFor(rm)

	// A second connector's material merges into the same map, so code and
	// documents produce one wiki whose pages can link across the boundary
	// rather than two wikis that cannot see each other.
	if f.docs != "" {
		docMap, docRouter, staging, err := syncDocs(ctx, out, f.docs)
		if staging != "" {
			defer os.RemoveAll(staging)
		}
		if err != nil {
			return err
		}
		merged, err := mapper.Merge(absPath, wm, docMap)
		if err != nil {
			return err
		}
		wm = merged
		maps.Copy(router.DocPaths, docRouter)
	}

	runner, err := agent.New(cfg)
	if err != nil {
		return err
	}

	pipeline := &jobs.Pipeline{
		Store: js, Runner: runner, Log: log,
		Budget: jobs.Budget{
			AnalyzeUSD: cfg.Agent.AnalyzeBudgetUSD,
			PageUSD:    cfg.Agent.PageBudgetUSD,
			RunUSD:     cfg.Agent.RunBudgetUSD,
			MaxPages:   cfg.Agent.MaxPagesPerRun,
		},
		Model:         cfg.Agent.Model,
		AnalyzeModel:  cfg.Agent.AnalyzeModel,
		FallbackModel: cfg.Agent.FallbackModel,
		Timeout:       cfg.Agent.Timeout,
		MaxRetries:    1,
	}

	req := jobs.BuildRequest{
		RunID:       newRunID(),
		WorkspaceID: workspaceID,
		Trigger:     "manual",
		Ref:         gitRef(rm),
		SourceDir:   absPath,
		Map:         wm,
		Router:      router,
		// Every unit is a candidate; the pipeline's hash gate is what actually
		// decides. A local path has no commit range to diff against, and the
		// gate is both cheaper and more precise than a filesystem comparison --
		// it compares each unit's content hash to what the last run recorded, so
		// a repeat build still costs nothing.
		Changes: diff.ChangeSet{FullRebuild: true},
		DryRun:  f.dryRun,
	}
	// The API runner returns pages as data, so a scratch directory is only
	// created for the CLI runner that writes files.
	if agent.WritesFiles(runner) {
		scratch, err := os.MkdirTemp("", "kiln-scratch-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(scratch)
		req.ScratchDir = scratch
	}

	started := time.Now()
	res, err := pipeline.Build(ctx, req)
	if err != nil {
		return err
	}

	return report(out, res, f.dryRun, time.Since(started))
}

// syncDocs ingests a documents directory through the upload connector and maps
// it with docmap, returning the map and the doc paths to add to routing.
//
// The returned staging directory holds the extracted text the unit inputs point
// at. It must outlive the build -- prompts are assembled from it -- so the
// caller owns removing it rather than a defer here.
func syncDocs(ctx context.Context, out io.Writer, dir string) (_ *mapper.WorkspaceMap, _ map[string]diff.Key, staging string, err error) {
	absDocs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, "", err
	}

	conn, err := connector.Get("upload")
	if err != nil {
		return nil, nil, "", err
	}
	fmt.Fprintf(out, "syncing via %s connector: %s\n", conn.Kind(), absDocs)

	staging, err = os.MkdirTemp("", "kiln-docs-")
	if err != nil {
		return nil, nil, "", err
	}

	set, err := conn.Sync(ctx, connector.Config{"path": absDocs}, staging)
	if err != nil {
		return nil, nil, staging, err
	}

	payload := upload.PayloadOf(set)
	if payload == nil {
		return nil, nil, staging, fmt.Errorf("upload connector returned no documents payload")
	}

	dm := &docmap.Mapper{}
	wm, err := dm.MapDocs(ctx, staging, payload.Docs)
	if err != nil {
		return nil, nil, staging, err
	}

	fmt.Fprintf(out, "  %d document(s), %d units\n", len(payload.Docs), len(wm.Units))
	// Skips are reported rather than swallowed: a folder that silently ingested
	// half its files would look like a working build.
	for _, s := range payload.Skipped {
		fmt.Fprintf(out, "  skipped %s: %s\n", s.Path, s.Reason)
	}

	routes := map[string]diff.Key{}
	for _, d := range payload.Docs {
		routes[d.Origin] = diff.DocKey(d.Origin)
	}
	return wm, routes, staging, nil
}

// routerFor maps module directories to their cache keys so a changed path is
// attributed to the module that owns it.
//
// Sub-partitions are registered alongside their parents. They are the reason a
// 15k-LOC repository becomes readable pages rather than one useless page, so
// leaving them out would mean a full rebuild covered only the top-level module.
// Attribution takes the longest matching prefix, so a change under
// internal/auth lands on that service rather than on the repository root.
func routerFor(rm *repomap.RepoMap) diff.Router {
	dirs := map[string]diff.Key{}
	for _, m := range rm.Modules {
		if !m.Empty {
			dirs[m.Dir] = diff.ModuleKey(m.Slug)
		}
	}

	docs := map[string]diff.Key{}
	for _, d := range rm.Docs {
		docs[d.Path] = diff.DocKey(d.Path)
	}

	return diff.Router{ModuleDirs: dirs, DocPaths: docs}
}

func report(out io.Writer, res *jobs.BuildResult, dryRun bool, elapsed time.Duration) error {
	if dryRun {
		fmt.Fprintf(out, "\nplan: %d unit(s), estimated $%.2f\n", len(res.Planned), res.EstimatedUSD)
		for _, k := range res.Planned {
			fmt.Fprintf(out, "  %s\n", k)
		}
		if res.Deferred > 0 {
			fmt.Fprintf(out, "\n%d more unit(s) exceed the per-run page cap and are not in this plan.\n",
				res.Deferred)
			fmt.Fprintln(out, "They stay stale until a later run; raise agent.max_pages_per_run to cover them in one.")
		}
		fmt.Fprintln(out, "\nno model calls were made (--dry-run)")
		return nil
	}

	s := res.Summary
	fmt.Fprintf(out, "\n%s in %s\n", s.Status, elapsed.Round(time.Millisecond))

	if s.Status == jobs.StatusNoChanges {
		// Worth stating plainly: this is the design working, not a failure.
		fmt.Fprintln(out, "  nothing changed; no model calls, no cost")
		return nil
	}

	fmt.Fprintf(out, "  pages: %d created, %d updated, %d removed\n", s.Created, s.Updated, s.Deleted)
	fmt.Fprintf(out, "  cost:  $%.4f across %d unit(s)\n", s.CostUSD, len(s.Items))

	for _, item := range s.Items {
		if item.Status != jobs.StatusSucceeded {
			fmt.Fprintf(out, "  FAILED %s: %s\n", item.Key, item.Err)
		}
	}
	if n := len(res.Violations); n > 0 {
		fmt.Fprintf(out, "\n%d validation violation(s):\n", n)
		for _, v := range res.Violations {
			fmt.Fprintf(out, "  %s\n", v)
		}
	}
	return nil
}

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}

func gitRef(rm *repomap.RepoMap) string {
	if rm.Git == nil {
		return ""
	}
	return short(rm.Git.HeadSHA)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
