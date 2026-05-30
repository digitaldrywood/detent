package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func main() {
	setupLoggerFromEnv(os.Stderr)

	ctx, stop := newSignalContext(context.Background())
	defer stop()

	if err := newRootCommand(ctx).ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("shutdown requested")
			return
		}

		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func newRootCommand(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "symphony",
		Short:        "Symphony agent orchestrator",
		Long:         "Symphony is an agent orchestrator for tracker-backed work queues.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.SetContext(ctx)

	return cmd
}

func newSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
