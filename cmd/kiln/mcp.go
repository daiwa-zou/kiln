package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	kilnmcp "github.com/daiwa-zou/kiln/internal/mcp"
	"github.com/daiwa-zou/kiln/internal/observability"
)

// newMCPCmd serves the wiki to agents over the Model Context Protocol.
//
// This role talks to kiln over HTTP rather than to Postgres, so it needs no
// database credentials and works against an instance running anywhere. The
// token it carries decides what it can read, which means an agent sees exactly
// the benches its token does.
func newMCPCmd(g *globalFlags) *cobra.Command {
	var (
		endpoint  string
		token     string
		workspace string
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the wiki to agents over the Model Context Protocol",
		Long: `Expose a kiln wiki to MCP-capable agents on stdio.

Tools: list_benches, search_wiki, read_page, wiki_overview, list_pages,
page_backlinks, wiki_gaps.

The server reads kiln over its HTTP API, so it needs a running instance and a
token with the read scope (unless auth.mode is "none"). Point an agent at it:

  {
    "mcpServers": {
      "kiln": {
        "command": "kiln",
        "args": ["mcp", "--url", "http://127.0.0.1:8080", "--workspace", "my-bench"],
        "env": {"KILN_TOKEN": "..."}
      }
    }
  }

With --workspace set, tools default to that bench and agents need not name one.
Without it, every call takes a "bench" argument and list_benches enumerates them.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "" {
				token = os.Getenv("KILN_TOKEN")
			}

			client := kilnmcp.NewClient(endpoint, token)
			srv := mcp.NewServer(&mcp.Implementation{
				Name:    "kiln",
				Title:   "kiln wiki",
				Version: observability.Version,
			}, nil)
			kilnmcp.NewServer(client, strings.TrimSpace(workspace), observability.Version).Register(srv)

			// stdio is the transport: stdout is the protocol channel, so
			// anything written there that is not JSON-RPC corrupts the session.
			// Diagnostics go to stderr, which is where the agent's host shows
			// server logs anyway.
			fmt.Fprintf(os.Stderr, "kiln mcp: serving %s on stdio\n", endpoint)
			err := srv.Run(cmd.Context(), &mcp.StdioTransport{})
			// A host closing the pipe is how an MCP session ends, not a
			// failure. Reporting it as one makes every clean disconnect look
			// like a crash in the host's server log.
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("mcp server: %w", err)
		},
	}

	f := cmd.Flags()
	f.StringVar(&endpoint, "url", "http://127.0.0.1:8080", "base URL of the kiln instance to read")
	f.StringVar(&token, "token", "", "API token with the read scope (default: $KILN_TOKEN)")
	f.StringVar(&workspace, "workspace", "", "default bench slug, so agents need not name one on every call")

	return cmd
}
