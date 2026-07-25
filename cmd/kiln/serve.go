package main

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/api"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

func newServeCmd(g *globalFlags) *cobra.Command {
	var (
		addr       string
		withWorker bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the API and the reading UI",
		Long: `Runs the HTTP API and the embedded UI.

--with-worker also runs the job worker in this process, which is the single
binary mode for a small deployment. At larger scale the worker runs as its own
replicas against the same queue.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			cfg, err := config.Load(config.Options{File: g.configFile, Role: config.RoleServer})
			if err != nil {
				return err
			}
			if addr != "" {
				cfg.HTTPAddr = addr
			}

			log := observability.NewLogger(firstNonEmpty(g.logLevel, cfg.LogLevel))

			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()

			// A server that boots against an unexpected schema fails much later
			// and much less clearly, so it refuses here instead.
			if err := store.CheckSchemaVersion(ctx, db.Pool); err != nil {
				return err
			}

			srv := &api.Server{
				Store: store.NewJobStore(db.Pool),
				DB:    db,
				Log:   log,
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "kiln %s listening on %s\n", observability.Version, cfg.HTTPAddr)
			if withWorker {
				// The worker loop lands with the job queue; today builds are
				// driven by `kiln build`, so this flag currently only affects
				// what the process reports.
				fmt.Fprintln(out, "  worker: in-process")
			}
			fmt.Fprintln(out, "  press ctrl-c to stop")

			return srv.Serve(ctx, cfg.HTTPAddr)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (overrides config)")
	cmd.Flags().BoolVar(&withWorker, "with-worker", false, "also run the job worker in this process")

	return cmd
}
