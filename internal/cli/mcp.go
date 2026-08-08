package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/mcp"
)

func newMCPCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve read-only operator tools over MCP stdio",
		Long: strings.TrimSpace(`Serve the shared read-only Detent operator catalog over MCP stdio.

The command connects to the already-running Detent daemon through its authenticated
HTTP read API. It never opens the runtime database or starts a daemon. Standard
output is reserved for newline-delimited MCP JSON-RPC frames; diagnostics use
standard error.`),
		Example: strings.TrimSpace(`detent mcp
detent --config ~/.config/detent/global.yaml mcp
DETENT_API_TOKEN=example detent --host 127.0.0.1 --port 4001 mcp`),
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newDashboardReadClient(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), opts)
			if err != nil {
				return err
			}
			return mcp.NewServer(client, opts.version).Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
