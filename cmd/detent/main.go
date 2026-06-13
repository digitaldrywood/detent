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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdownController := cli.NewShutdownController()
	stopSignals := notifyShutdownRequests(shutdownController, cancel)
	defer stopSignals()

	cmd := newRootCommand(ctx, shutdownController)
	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(handleCommandError(cmd, err))
	}
}

func handleCommandError(cmd *cobra.Command, err error) int {
	code := cli.ExitCode(err)
	if code == cli.ExitSuccess {
		slog.Info("shutdown requested")
		return code
	}
	if errors.Is(err, cli.ErrShutdownForced) || errors.Is(err, cli.ErrShutdownTimeout) {
		return code
	}

	var errOut io.Writer = os.Stderr
	if cmd != nil {
		errOut = cmd.ErrOrStderr()
	}
	if cli.CommandOutputIsJSON(cmd) {
		if writeErr := cli.WriteCommandErrorJSON(errOut, err); writeErr != nil {
			fmt.Fprintf(errOut, "Error: %v\n", writeErr)
		}
		return code
	}
	fmt.Fprintf(errOut, "Error: %v\n", err)
	return code
}

func newRootCommand(ctx context.Context, shutdownControllers ...*cli.ShutdownController) *cobra.Command {
	build := currentBuildInfo()
	opts := []cli.Option{
		cli.WithVersion(build.Version),
		cli.WithBuild(build),
		cli.WithLoggerFunc(setupLoggerFromRuntime),
	}
	if len(shutdownControllers) > 0 && shutdownControllers[0] != nil {
		opts = append(opts, cli.WithShutdownController(shutdownControllers[0]))
	}
	cmd := cli.NewRootCommand(ctx, opts...)
	cmd.Version = build.Version
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.AddCommand(
		newVersionCommand(),
		newUpdateCommand(ctx, newDefaultUpdateRunner),
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
		Args:         cli.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if inputPath == "" {
				return cli.ValidationError("shadow-run --input is required")
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
