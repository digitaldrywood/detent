package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/symphony-go/internal/cli"
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
	return cli.NewRootCommand(ctx)
}

func newSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
