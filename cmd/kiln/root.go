package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/daiwa-zou/kiln/internal/observability"
)

// globalFlags are bound on the root command and available to every subcommand.
type globalFlags struct {
	configFile string
	logLevel   string
	jsonOutput bool
}

func newRootCmd() *cobra.Command {
	var g globalFlags

	cmd := &cobra.Command{
		Use:   "kiln",
		Short: "Build and maintain knowledge bases from code, documents, and the web",
		Long: `kiln compiles your sources into a persistent, interlinked wiki and keeps it
current as they change. Only the pages affected by a change are regenerated, so a
run over unchanged sources costs nothing.`,
		SilenceUsage:  true,
		SilenceErrors: false,
		// Roles that need a server or database are wired in their own files.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	pf := cmd.PersistentFlags()
	pf.StringVar(&g.configFile, "config", "", "path to config file (default: $XDG_CONFIG_HOME/kiln/config.toml)")
	pf.StringVar(&g.logLevel, "log-level", "", "log level: debug, info, warn, error")
	pf.BoolVar(&g.jsonOutput, "json", false, "emit machine-readable output")

	cmd.AddCommand(newVersionCmd())

	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the kiln version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), observability.Version)
			return err
		},
	}
}
