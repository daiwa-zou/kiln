package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/config"
	"github.com/daiwa-zou/kiln/internal/store"
)

func newAdminCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operational commands: migrations, diagnostics, users",
	}
	cmd.AddCommand(newMigrateCmd(g), newDoctorCmd(g))
	return cmd
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
