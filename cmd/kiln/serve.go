package main

import (
	"fmt"
	"net"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/api"
	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/blob"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/github"
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

			// With auth disabled every caller is an admin. On loopback that
			// is a personal-machine convenience; on any reachable interface
			// it hands the whole instance to the network. Refusing to start
			// is the only honest behavior.
			if cfg.Auth.Mode == config.AuthNone && !loopbackAddr(cfg.HTTPAddr) {
				return fmt.Errorf(
					"refusing to serve %s with auth.mode = \"none\": every caller would be an admin.\n"+
						"Bind a loopback address (e.g. 127.0.0.1:8080) or enable token auth",
					cfg.HTTPAddr)
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

			// One registry per process. With --with-worker the API and the
			// worker record into the same one and a single listener exposes
			// both, rather than two halves of the picture fighting over a
			// port.
			metrics := observability.NewMetrics()

			ws := store.NewWikiStore(db.Pool)
			srv := &api.Server{
				Store:                  ws,
				Writes:                 ws,
				Runs:                   ws,
				Admin:                  ws,
				Members:                ws,
				Files:                  ws,
				Figures:                ws,
				Pages:                  ws,
				Publish:                ws,
				Workspaces:             ws,
				BudgetWindow:           cfg.Agent.BudgetWindow,
				SourcePollInterval:     cfg.Worker.SourcePollInterval,
				DB:                     db,
				Log:                    log,
				CORSOrigins:            cfg.CORSOrigins,
				PublicURL:              cfg.PublicURL,
				MCPAuthorizationServer: cfg.MCPAuthorizationServer,
				Metrics:                metrics,
			}
			// Without object storage the upload route answers 503 with the
			// fix; file listing and deletion keep working.
			if blobs, err := blob.Open(cfg.Storage); err != nil {
				log.Warn("object storage unavailable; uploads disabled", "err", err)
			} else {
				srv.Blobs = blobs
				fmt.Fprintln(cmd.OutOrStdout(), "  document uploads enabled ("+string(cfg.Storage.Backend)+" storage)")
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
			gh := &github.Client{
				ClientID:     cfg.GitHub.ClientID,
				ClientSecret: cfg.Secrets.GitHubClientSecret,
				AppID:        cfg.GitHub.AppID,
				PrivateKey:   []byte(cfg.Secrets.GitHubPrivateKey),
				BaseURL:      cfg.GitHub.BaseURL,
				APIBaseURL:   cfg.GitHub.APIBaseURL,
			}
			if gh.SignInConfigured() {
				srv.GitHub = gh
				srv.Users = ws
				srv.SessionPool = db.Pool
				srv.SessionTTL = cfg.GitHub.SessionTTL
				fmt.Fprintln(cmd.OutOrStdout(), "  GitHub sign-in enabled")
			}
			if cfg.Secrets.GitHubWebhookSecret != "" {
				srv.Hooks = ws
				srv.WebhookSecret = []byte(cfg.Secrets.GitHubWebhookSecret)
				srv.WebhookCooldown = cfg.GitHub.WebhookCooldown
				fmt.Fprintln(cmd.OutOrStdout(), "  GitHub webhook ingress enabled at /hooks/github")
			}

			if cfg.Auth.Mode == config.AuthNone {
				log.Warn("API authentication is disabled (auth.mode = none); every bench is readable by anyone who can reach this port")
			} else {
				source := &auth.PGSource{Pool: db.Pool}
				m := &auth.Middleware{Source: source, Log: log}
				if gh.SignInConfigured() {
					// Cookie sessions ride the same identity pipeline as
					// bearer tokens; CSRF is enforced inside the middleware.
					m.Sessions = source
				}
				srv.Auth = m
			}

			out := cmd.OutOrStdout()
			scheme := "http"
			if cfg.TLSCert != "" && cfg.TLSKey != "" {
				scheme = "https"
			}
			fmt.Fprintf(out, "kiln %s listening on %s (%s)\n", observability.Version, cfg.HTTPAddr, scheme)
			// The MCP endpoint is the one route whose address a person has to
			// paste somewhere else, so say it rather than make them derive it.
			mcpURL := strings.TrimRight(cfg.PublicURL, "/")
			if mcpURL == "" {
				mcpURL = scheme + "://" + cfg.HTTPAddr
			}
			fmt.Fprintf(out, "  MCP endpoint at %s/mcp\n", mcpURL)
			// Metrics listen on their own port so the API's ingress never
			// publishes them. Failing to bind must not take the server down:
			// serving the wiki matters more than reporting on it.
			var metricsDone chan struct{}
			if cfg.MetricsAddr != "" {
				fmt.Fprintf(out, "  metrics at %s/metrics\n", cfg.MetricsAddr)
				metricsDone = make(chan struct{})
				go func() {
					defer close(metricsDone)
					if err := metrics.ServeMetrics(ctx, cfg.MetricsAddr); err != nil {
						log.Error("metrics listener stopped", "error", err)
					}
				}()
			}

			var workerDone chan struct{}
			if withWorker {
				w, err := newWorker(cfg, db, log)
				if err != nil {
					return err
				}
				w.Metrics = metrics
				fmt.Fprintln(out, "  build worker running in-process (--with-worker)")
				workerDone = make(chan struct{})
				go func() {
					defer close(workerDone)
					_ = w.Run(ctx)
				}()
			}
			fmt.Fprintln(out, "  press ctrl-c to stop")

			err = srv.Serve(ctx, cfg.HTTPAddr, cfg.TLSCert, cfg.TLSKey)
			if metricsDone != nil {
				<-metricsDone
			}
			if workerDone != nil {
				// The worker drains: it stops claiming immediately but may
				// hold an in-flight build for its grace window, and exiting
				// before it finishes would race the requeue write.
				fmt.Fprintln(out, "  waiting for the worker to drain…")
				<-workerDone
			}
			return err
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (overrides config)")
	cmd.Flags().BoolVar(&withWorker, "with-worker", false, "also run a build worker in this process")

	return cmd
}

// loopbackAddr reports whether a listen address can only be reached from
// this machine. An empty host (":8080") binds every interface and is not
// loopback.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
