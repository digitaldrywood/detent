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
	os.Exit(runCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	setupLoggerFromEnv(stdout, stderr)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	shutdownController := cli.NewShutdownController()
	stopSignals := notifyShutdownRequests(shutdownController, cancel, stderr, os.Exit)
	defer stopSignals()

	cmd := newRootCommand(ctx, shutdownController)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)
	cmd.SilenceErrors = true
	format, formatErr := cli.ResolveOutputFormatFromArgs(args, os.Getenv, cli.WriterIsTTY(stdout))
	if err := cmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("shutdown requested")
			return cli.ExitSuccess
		}
		return renderCommandError(cmd, err, stderr, format, formatErr)
	}
	return cli.ExitSuccess
}

func renderCommandError(cmd *cobra.Command, err error, stderr io.Writer, format cli.OutputFormat, formatErr error) int {
	problem := cli.ProblemForCommandError(cmd, err)
	if formatErr == nil && format == cli.OutputFormatJSON {
		if writeErr := cli.WriteProblemJSON(stderr, problem); writeErr != nil {
			slog.Error("write problem response", "error", writeErr)
		}
		return problem.ExitCode
	}
	if writeErr := writePrettyCommandError(stderr, err, problem); writeErr != nil {
		slog.Error("write error response", "error", writeErr)
	}
	return problem.ExitCode
}

func writePrettyCommandError(stderr io.Writer, err error, problem cli.Problem) error {
	if stderr == nil {
		return nil
	}
	if writeErr := cli.WritePrettyError(stderr, err); writeErr != nil {
		return writeErr
	}
	if cli.ErrorHint(err) != "" || problem.SuggestedFix == "" {
		return nil
	}
	_, writeErr := fmt.Fprintf(stderr, "Hint: %s\n", problem.SuggestedFix)
	return writeErr
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
	cli.ConfigureExamplesFirstHelp(cmd)
	cli.ConfigureCommandSuggestions(cmd)

	return cmd
}

func newShadowRunCommand() *cobra.Command {
	var inputPath string
	var allowDiff bool

	cmd := &cobra.Command{
		Use:          "shadow-run",
		Short:        "Compare read-only Go decisions with an Elixir shadow report",
		Args:         cli.NoArgs,
		Example:      "detent shadow-run --input ./shadow.json --allow-diff",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if inputPath == "" {
				return cli.NewValidationError("shadow-run --input is required", "Run detent shadow-run --input ./shadow.json.", nil)
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
