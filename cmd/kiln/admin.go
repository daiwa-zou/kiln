package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/crypto"
	"github.com/daiwa-zou/kiln/internal/store"
)

func newAdminCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operational commands: migrations, diagnostics, tokens",
	}
	cmd.AddCommand(newMigrateCmd(g), newDoctorCmd(g), newTokenCmd(g), newRotateKeyCmd(g),
		newHealthCmd())
	return cmd
}

// newHealthCmd probes a running server's readiness endpoint. It exists so a
// container healthcheck is `kiln admin health` rather than a curl the image
// would otherwise have to ship purely to check on itself -- the same reason
// distroless deployments end up with bespoke probe binaries. It needs no
// configuration and no database of its own: the server it asks already has
// both.
func newHealthCmd() *cobra.Command {
	var (
		addr    string
		timeout time.Duration
		live    bool
	)
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Probe a running server's health endpoint",
		Long: `Exits 0 when the server is ready and non-zero otherwise, for container
healthchecks and orchestrator probes.

Readiness (the default) reports whether the database is reachable, which is
what should gate traffic. --live probes liveness instead: it answers even
while the database is down, so an orchestrator does not kill a process that is
merely waiting on Postgres.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := "/readyz"
			if live {
				path = "/healthz"
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				strings.TrimSuffix(addr, "/")+path, nil)
			if err != nil {
				return err
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("kiln: health probe: %w", err)
			}
			defer res.Body.Close()
			// Bound the read: a probe must not be a memory sink if something
			// answers this path with a stream.
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))

			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("kiln: %s returned %s: %s",
					path, res.Status, strings.TrimSpace(string(body)))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s OK\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "http://127.0.0.1:8080", "base URL of the server to probe")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "probe timeout")
	cmd.Flags().BoolVar(&live, "live", false, "probe liveness (/healthz) instead of readiness (/readyz)")
	return cmd
}

func newRotateKeyCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-key",
		Short: "Re-seal every credential under a new master key",
		Long: `Reads the current master key from KILN_MASTER_KEY and the replacement from
KILN_NEW_MASTER_KEY, then rewrites every credential in one transaction:
either all of them move to the new key or none do. Afterwards, restart every
kiln process with the new key as KILN_MASTER_KEY.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(config.Options{File: g.configFile})
			if err != nil {
				return err
			}
			oldKeyring, err := crypto.NewKeyring(cfg.Secrets.MasterKey)
			if err != nil {
				return fmt.Errorf("current master key: %w", err)
			}
			newKey, err := readNewMasterKey()
			if err != nil {
				return err
			}
			newKeyring, err := crypto.NewKeyring(newKey)
			if err != nil {
				return fmt.Errorf("new master key: %w", err)
			}

			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()

			n, err := store.NewWikiStore(db.Pool).ResealCredentials(ctx, oldKeyring.Open, newKeyring.Seal)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "re-sealed %d credential(s) under the new key\n", n)
			fmt.Fprintln(out, "now restart every kiln process with KILN_MASTER_KEY set to the new key")
			return nil
		},
	}
}

// readNewMasterKey resolves KILN_NEW_MASTER_KEY with the same _FILE
// indirection every other secret supports.
func readNewMasterKey() (string, error) {
	if path := strings.TrimSpace(os.Getenv("KILN_NEW_MASTER_KEY_FILE")); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read KILN_NEW_MASTER_KEY_FILE: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	key := os.Getenv("KILN_NEW_MASTER_KEY")
	if key == "" {
		return "", fmt.Errorf("set KILN_NEW_MASTER_KEY (or _FILE) to the replacement key")
	}
	return key, nil
}

func newTokenCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Mint and revoke API bearer tokens",
	}

	var (
		login  string
		org    string
		name   string
		scopes string
		admin  bool
		ttl    time.Duration
	)

	create := &cobra.Command{
		Use:   "create",
		Short: "Mint a bearer token, creating the user and org membership as needed",
		Long: `Mints a token and prints it exactly once; only its hash is stored. Pass the
token as "Authorization: Bearer <token>" on API requests, or paste it into the
reading UI when prompted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(config.Options{File: g.configFile})
			if err != nil {
				return err
			}
			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()

			plain, err := auth.Mint(ctx, db.Pool, auth.MintRequest{
				Login:  login,
				Org:    org,
				Name:   name,
				Scopes: splitScopes(scopes),
				Admin:  admin,
				TTL:    ttl,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s\n", plain)
			fmt.Fprintln(cmd.ErrOrStderr(), "shown once; store it now -- only its hash is kept")
			return nil
		},
	}
	create.Flags().StringVar(&login, "login", "", "user login the token belongs to (required)")
	create.Flags().StringVar(&org, "org", "local", "org to grant membership in (empty to skip)")
	create.Flags().StringVar(&name, "name", "default", "token name, for identification and revocation")
	create.Flags().StringVar(&scopes, "scopes", "read", "comma-separated scopes")
	create.Flags().BoolVar(&admin, "admin", false, "grant the user admin (visibility into every org)")
	create.Flags().DurationVar(&ttl, "ttl", 0, "expiry, e.g. 720h (0 = never)")
	_ = create.MarkFlagRequired("login")

	var (
		revokeLogin string
		revokeName  string
	)
	revoke := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a user's named tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(config.Options{File: g.configFile})
			if err != nil {
				return err
			}
			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()

			n, err := auth.Revoke(ctx, db.Pool, revokeLogin, revokeName)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %d token(s)\n", n)
			return nil
		},
	}
	revoke.Flags().StringVar(&revokeLogin, "login", "", "user login (required)")
	revoke.Flags().StringVar(&revokeName, "name", "default", "token name")
	_ = revoke.MarkFlagRequired("login")

	cmd.AddCommand(create, revoke)
	return cmd
}

func splitScopes(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newMigrateCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		Long: `Applies every unapplied migration inside a Postgres advisory lock, so
concurrent invocations block rather than racing. Migrations are never applied
implicitly on server boot; run this as a one-shot step before starting servers.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(config.Options{File: g.configFile})
			if err != nil {
				return err
			}

			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()

			before, err := store.CurrentVersion(ctx, db.Pool)
			if err != nil {
				return err
			}

			applied, err := store.Migrate(ctx, db.Pool)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if len(applied) == 0 {
				fmt.Fprintf(out, "schema already at version %d, nothing to do\n", before)
				return nil
			}
			fmt.Fprintf(out, "applied %d migration(s): %v\n", len(applied), applied)
			fmt.Fprintf(out, "schema now at version %d\n", applied[len(applied)-1])
			return nil
		},
	}
}

func newDoctorCmd(g *globalFlags) *cobra.Command {
	var probe bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate configuration and dependencies",
		Long: `Checks the whole configuration surface in one pass rather than failing one
problem per restart: database reachability, schema version, and the master key.
Further checks (object storage, GitHub App credentials, the claude binary)
land as their subsystems do.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			cfg, err := config.Load(config.Options{File: g.configFile})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "config      OK  (%s)\n", cfg.Database.Redacted())

			db, err := store.Open(ctx, cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			fmt.Fprintln(out, "postgres    OK")

			version, err := store.CurrentVersion(ctx, db.Pool)
			if err != nil {
				return err
			}
			if err := store.CheckSchemaVersion(ctx, db.Pool); err != nil {
				fmt.Fprintf(out, "schema      FAIL (at %d, want %d)\n", version, store.SchemaVersion)
				return err
			}
			fmt.Fprintf(out, "schema      OK  (version %d)\n", version)

			// The master key gates credentials: absent is fine (a local-path
			// deployment needs none), but a present key that cannot build a
			// keyring means every credential write and clone would fail.
			switch cfg.Secrets.MasterKey {
			case "":
				fmt.Fprintln(out, "master key  absent (credentials unavailable; set KILN_MASTER_KEY to enable)")
			default:
				if _, err := crypto.NewKeyring(cfg.Secrets.MasterKey); err != nil {
					fmt.Fprintf(out, "master key  FAIL (%v)\n", err)
					return err
				}
				fmt.Fprintln(out, "master key  OK")
			}

			// The CLI runner is the one agent configuration that can fail for a
			// reason outside kiln entirely -- a missing binary, or a session
			// that expired since the last build. Both are free to detect and
			// expensive to discover late: a worker with neither accepts a run,
			// plans it, and fails every unit on authentication after the queue
			// has claimed it. Checked only when it is the selected runner,
			// since the binary is irrelevant otherwise.
			if cfg.Agent.Runner == config.RunnerCLI {
				h, err := agent.CheckCLI(ctx, cfg.Agent.Binary, cfg.Secrets.AnthropicAPIKey, cfg.Agent.BaseURL)
				switch {
				case err != nil:
					fmt.Fprintf(out, "claude cli  FAIL (%v)\n", err)
					return err
				case !h.Usable() && h.AuthErr == nil:
					// A definite "not logged in" is a failure: every build
					// would fail, and saying so here is the whole point.
					fmt.Fprintf(out, "claude cli  FAIL (%s)\n", h.Summary())
					return fmt.Errorf("agent: the claude CLI is not authenticated; run `claude auth login`")
				default:
					fmt.Fprintf(out, "claude cli  OK  (%s)\n", h.Summary())
				}

				// The check above reads the CLI's stored opinion of itself,
				// which is conclusive when it says no and merely hopeful when
				// it says yes: a session whose access token is still inside its
				// window reports as logged in even when the refresh token
				// behind it is dead. Only a real call finds that out, so it is
				// opt-in -- this one spends a cent or two.
				if probe {
					if err := agent.ProbeCLI(ctx, cfg.Agent.Binary, cfg.Secrets.AnthropicAPIKey, cfg.Agent.BaseURL); err != nil {
						fmt.Fprintf(out, "claude call FAIL (%v)\n", err)
						return err
					}
					fmt.Fprintln(out, "claude call OK  (completed a live request)")
				}
			}

			// Remaining checks land as their subsystems do: storage round-trip,
			// GitHub App JWT, Anthropic key.
			return nil
		},
	}

	cmd.Flags().BoolVar(&probe, "probe", false,
		"additionally make one small live model call, the only conclusive test that generation would work")
	return cmd
}
