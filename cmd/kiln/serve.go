package main

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/api"
	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/crypto"
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

The API requires a bearer token by default (mint one with
"kiln admin token create"); set auth.mode = "none" to opt out on a
single-user localhost deployment.

--with-worker also runs a build worker in this process, for single-node
deployments and local development; production deployments run "kiln worker"
processes separately so builds scale independently of the API.`,
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
				Store:        ws,
				Writes:       ws,
				Runs:         ws,
				Admin:        ws,
				BudgetWindow: cfg.Agent.BudgetWindow,
				DB:           db,
				Log:          log,
				CORSOrigins:  cfg.CORSOrigins,
			}
			// Without a master key the credential routes answer 503 with the
			// fix; connector CRUD keeps working for local-path setups.
			if cfg.Secrets.MasterKey != "" {
				keyring, err := crypto.NewKeyring(cfg.Secrets.MasterKey)
				if err != nil {
					return err
				}
				srv.Keyring = keyring
			}
			if cfg.Auth.Mode == config.AuthNone {
				log.Warn("API authentication is disabled (auth.mode = none); every bench is readable by anyone who can reach this port")
			} else {
				srv.Auth = &auth.Middleware{Source: &auth.PGSource{Pool: db.Pool}, Log: log}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "kiln %s listening on %s\n", observability.Version, cfg.HTTPAddr)
			if withWorker {
				w, err := newWorker(cfg, db, log)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, "  build worker running in-process (--with-worker)")
				go func() { _ = w.Run(ctx) }()
			}
			fmt.Fprintln(out, "  press ctrl-c to stop")

			return srv.Serve(ctx, cfg.HTTPAddr)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (overrides config)")
	cmd.Flags().BoolVar(&withWorker, "with-worker", false, "also run a build worker in this process")

	return cmd
}
