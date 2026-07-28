package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/mapper/repomap"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

type buildFlags struct {
	path      string
	docs      string
	web       []string
	workspace string
	org       string
	since     string
	dryRun    bool
	full      bool
	budget    float64
	model     string
}

func newBuildCmd(g *globalFlags) *cobra.Command {
	var f buildFlags

	cmd := &cobra.Command{
		Use:   "build [path]",
		Short: "Build or refresh a bench's wiki from a local directory",
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
	fl.StringArrayVar(&f.web, "web", nil, "also ingest this https URL into the same wiki (repeatable)")
	fl.StringVar(&f.since, "since", "", "route changes from this git ref instead of considering every unit")
	fl.StringVar(&f.workspace, "bench", "", "bench slug (defaults to the directory name)")
	fl.StringVar(&f.workspace, "workspace", "", "deprecated alias for --bench")
	_ = fl.MarkDeprecated("workspace", "use --bench")
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

	// A repository is optional when documents or web pages feed the build; the
	// slug then has no directory name to fall back on, so --bench must say it.
	if f.path == "" && f.docs == "" && len(f.web) == 0 {
		return fmt.Errorf("nothing to build: give a directory (kiln build <path>), --docs, or --web")
	}
	absPath := ""
	if f.path != "" {
		var err error
		absPath, err = filepath.Abs(f.path)
		if err != nil {
			return err
		}
		if info, err := os.Stat(absPath); err != nil || !info.IsDir() {
			return fmt.Errorf("%s is not a directory", f.path)
		}
	}

	slug := f.workspace
	if slug == "" {
		if absPath == "" {
			return fmt.Errorf("--bench is required when building without a repository directory")
		}
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

	js := store.NewWikiStore(db.Pool)
	name := slug
	if absPath != "" {
		name = filepath.Base(absPath)
	}
	workspaceID, err := js.EnsureWorkspace(ctx, f.org, slug, name)
	if err != nil {
		return err
	}

	runner, err := agent.New(cfg)
	if err != nil {
		return err
	}
	pipeline := jobs.NewPipeline(cfg, js, runner, log)
	// A CLI build can drop blob-backed sources a worker imported earlier; with
	// storage configured, their released blobs are cleaned up here too.
	if blobs, err := blob.Open(cfg.Storage); err == nil {
		pipeline.Blobs = blobs
	}

	started := time.Now()
	res, err := pipeline.Execute(ctx, jobs.ExecuteRequest{
		RunID:       jobs.NewRunID(),
		WorkspaceID: workspaceID,
		Trigger:     "manual",
		Source:      jobs.SourceSpec{Path: absPath, DocsDir: f.docs, WebURLs: f.web, Slug: slug},
		BaseRef:     f.since,
		Force:       f.full,
		DryRun:      f.dryRun,
		Progress:    out,
	})
	if err != nil {
		return err
	}

	return report(out, res, f.dryRun, time.Since(started))
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
