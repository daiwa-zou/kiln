package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/auth"
	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/store"
)

func newAdminCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operational commands: migrations, diagnostics, tokens",
	}
	cmd.AddCommand(newMigrateCmd(g), newDoctorCmd(g), newTokenCmd(g))
	return cmd
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
	return &cobra.Command{
		Use:   "doctor",
		Short: "Validate configuration and dependencies",
		Long: `Checks the whole configuration surface in one pass rather than failing one
problem per restart: database reachability and schema version, object storage,
the master key, and the claude binary.`,
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

			// Remaining checks land as their subsystems do: storage round-trip,
			// master-key decryption, GitHub App JWT, Anthropic key, claude binary.
			return nil
		},
	}
}
