package cli

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type hubRunFunc func(context.Context, hubserver.Config) error

func newHubCommand(opts options) *cobra.Command {
	return newHubCommandWithRun(opts.version, hubserver.Run)
}

func newHubCommandWithRun(version string, run hubRunFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hub",
		Short:   "Run the shared Detent Hub service",
		Example: "detent hub serve --database /var/lib/detent/hub.db",
		Args:    NoArgs,
	}
	cmd.AddCommand(newHubServeCommand(version, run))
	return cmd
}

func newHubServeCommand(version string, run hubRunFunc) *cobra.Command {
	var databasePath string
	var listenAddress string
	var busyTimeout time.Duration
	var shutdownTimeout time.Duration

	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve the Detent Hub health endpoint",
		Example:      "detent hub serve --database /var/lib/detent/hub.db --listen 127.0.0.1:7777",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := OutputForCommand(cmd); err != nil {
				return err
			}
			if strings.TrimSpace(databasePath) == "" {
				return NewValidationError("hub database path is required", "Run detent hub serve --database /path/to/hub.db.", nil)
			}
			if strings.TrimSpace(listenAddress) == "" {
				return NewValidationError("hub listen address is required", "Use --listen 127.0.0.1:7777 or another explicit address.", nil)
			}
			return run(cmd.Context(), hubserver.Config{
				DatabasePath:    databasePath,
				ListenAddress:   listenAddress,
				BusyTimeout:     busyTimeout,
				ShutdownTimeout: shutdownTimeout,
				Logger:          slog.Default(),
				Version:         version,
			})
		},
	}
	cmd.Flags().StringVar(&databasePath, "database", "", "local filesystem path to the Hub SQLite database")
	cmd.Flags().StringVar(&listenAddress, "listen", hubserver.DefaultListenAddress, "Hub listen address")
	cmd.Flags().DurationVar(&busyTimeout, "busy-timeout", 5*time.Second, "SQLite busy timeout")
	cmd.Flags().DurationVar(&shutdownTimeout, "shutdown-timeout", 5*time.Second, "graceful HTTP shutdown timeout")
	return cmd
}
