package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/cli"
	"github.com/digitaldrywood/detent/internal/shadow"
)

func main() {
	setupLoggerFromEnv(os.Stdout, os.Stderr)

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
	build := currentBuildInfo()
	cmd := cli.NewRootCommand(ctx,
		cli.WithVersion(build.Version),
		cli.WithBuild(build),
		cli.WithLoggerFunc(setupLoggerFromRuntime),
	)
	cmd.Version = build.Version
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.AddCommand(
		newVersionCommand(),
		newUpdateCommand(newDefaultUpdateRunner),
		newShadowRunCommand(),
	)

	return cmd
}

func newShadowRunCommand() *cobra.Command {
	var inputPath string
	var allowDiff bool

	cmd := &cobra.Command{
		Use:          "shadow-run",
		Short:        "Compare read-only Go decisions with an Elixir shadow report",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if inputPath == "" {
				return errors.New("shadow-run --input is required")
			}
			return runShadowRun(cmd.OutOrStdout(), inputPath, allowDiff)
		},
	}
	cmd.Flags().StringVar(&inputPath, "input", "", "Path to shadow-run JSON input")
	cmd.Flags().BoolVar(&allowDiff, "allow-diff", false, "Return success even when differences are found")
	return cmd
}

func runShadowRun(out io.Writer, inputPath string, allowDiff bool) error {
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read shadow-run input: %w", err)
	}

	var input shadow.Input
	if err := json.Unmarshal(raw, &input); err != nil {
		return fmt.Errorf("decode shadow-run input: %w", err)
	}

	report, err := shadow.Run(input)
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write shadow-run report: %w", err)
	}
	if report.HasDifferences() && !allowDiff {
		return shadow.ErrDifferences
	}
	return nil
}
