package main

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/api"
	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/observability"
	"github.com/daiwa-zou/kiln/internal/store"
)

func newServeCmd(g *globalFlags) *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the API and the reading UI",
		Long: `Runs the HTTP API and the embedded UI.

The API requires a bearer token by default (mint one with
"kiln admin token create"); set auth.mode = "none" to opt out on a
single-user localhost deployment.`,
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

			ws := store.NewWikiStore(db.Pool)
			srv := &api.Server{
				Store:       ws,
				Writes:      ws,
				DB:          db,
				Log:         log,
				CORSOrigins: cfg.CORSOrigins,
			}
			if cfg.Auth.Mode == config.AuthNone {
				log.Warn("API authentication is disabled (auth.mode = none); every bench is readable by anyone who can reach this port")
			} else {
				srv.Auth = &auth.Middleware{Source: &auth.PGSource{Pool: db.Pool}, Log: log}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "kiln %s listening on %s\n", observability.Version, cfg.HTTPAddr)
			fmt.Fprintln(out, "  press ctrl-c to stop")

			return srv.Serve(ctx, cfg.HTTPAddr)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (overrides config)")

	return cmd
}
