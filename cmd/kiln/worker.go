package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/jobs"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
	"github.com/daiwa-zou/kiln/internal/worker"
)

func newWorkerCmd(g *globalFlags) *cobra.Command {
	var once bool

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Claim queued runs and build them",
		Long: `Runs the build worker: claims queued runs from the database and executes
them through the same pipeline "kiln build" uses.

Each worker claims one run at a time; run more worker processes to build more
benches in parallel. Local source paths from database-configured connectors
are only readable under worker.permitted_source_roots.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfg, err := config.Load(config.Options{File: g.configFile, Role: config.RoleWorker})
			if err != nil {
				return err
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

			w, err := newWorker(cfg, db, log)
			if err != nil {
				return err
			}

			// A standalone worker holds no API listener, so this port is the
			// only way to see queue depth, build outcomes, and spend from
			// outside. Skipped for --once, which is a one-shot drain that
			// would exit before a scrape ever arrived.
			if cfg.MetricsAddr != "" && !once {
				w.Metrics = observability.NewMetrics()
				fmt.Fprintf(cmd.OutOrStdout(), "metrics at %s/metrics\n", cfg.MetricsAddr)
				go func() {
					if err := w.Metrics.ServeMetrics(ctx, cfg.MetricsAddr); err != nil {
						log.Error("metrics listener stopped", "error", err)
					}
				}()
			}

			if once {
				n, err := w.RunOnce(ctx)
				fmt.Fprintf(cmd.OutOrStdout(), "processed %d run(s)\n", n)
				return err
			}
			if err := w.Run(ctx); errors.Is(err, context.Canceled) {
				return nil
			} else {
				return err
			}
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "drain the queue and exit instead of polling")

	return cmd
}

// newWorker wires a worker from an open database, shared by "kiln worker" and
// "kiln serve --with-worker".
func newWorker(cfg *config.Config, db *store.DB, log *slog.Logger) (*worker.Worker, error) {
	runner, err := agent.New(cfg)
	if err != nil {
		return nil, err
	}

	// A worker on the CLI runner with no logged-in session will claim runs and
	// fail every unit of them on authentication -- after the queue has taken
	// the work and a human has watched it start. Both causes are free to detect
	// here, so say so at boot instead.
	//
	// A warning rather than a refusal to start: the session can be renewed
	// without restarting kiln, and a worker that exits on a condition that may
	// fix itself in a minute is worse than one that says what is wrong. The
	// hard version of this check is `kiln admin doctor`, which is where an
	// operator asks the question deliberately.
	if cfg.Agent.Runner == config.RunnerCLI {
		switch h, err := agent.CheckCLI(context.Background(), cfg.Agent.Binary, cfg.Secrets.AnthropicAPIKey, cfg.Agent.BaseURL); {
		case err != nil:
			log.Warn("the claude CLI is unusable; every build will fail until it is fixed", "err", err)
		case !h.Usable() && h.AuthErr == nil:
			log.Warn("the claude CLI is not authenticated; every build will fail until it is",
				"fix", "claude auth login", "version", h.Version)
		default:
			log.Info("claude CLI ready", "path", h.Path, "status", h.Summary())
		}
	}

	js := store.NewWikiStore(db.Pool)
	pipeline := jobs.NewPipeline(cfg, js, runner, log)
	w := worker.New(cfg, js, pipeline, log)
	// Without object storage the worker still builds path-mode and git
	// sources; a files-mode upload connector then fails its run with the
	// configuration message rather than the worker refusing to start.
	if blobs, err := blob.Open(cfg.Storage); err != nil {
		log.Warn("object storage unavailable; uploaded-files sources disabled", "err", err)
	} else {
		w.Blobs = blobs
		// The pipeline deletes released blobs after an approved deletion's
		// cascade lands, closing the loop the file API's upload opened.
		pipeline.Blobs = blobs
	}
	return w, nil
}
