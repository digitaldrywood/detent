package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/cli"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/service"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == toolcache.ProgramArgument {
		if err := toolcache.RunProgram(os.Getenv("GOCACHE"), os.TempDir(), os.Stdin, os.Stdout); err != nil {
			slog.Error("managed Go cache program failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == cli.SSHProbeArgument {
		fmt.Fprintln(os.Stdout, runnerpkg.SSHProtocolVersion)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == cli.SSHWorkerArgument {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		if err := cli.RunSSHWorker(context.Background(), os.Stdin, os.Stdout, logger); err != nil {
			os.Exit(cli.ExitGeneral)
		}
		return
	}
	if gobudget.IsWrapperInvocation(os.Args[0]) {
		os.Exit(gobudget.RunWrapper(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	if gotestreceipt.IsWrapperInvocation(os.Args[0]) {
		os.Exit(gotestreceipt.RunWrapper(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	exitCode, restartBinary := runCLIResultWithRestart(context.Background(), os.Args[1:], os.Stdout, os.Stderr, automaticRestartRequest())
	manager := service.ManagerFromProcessEnvironment(runtime.GOOS, os.LookupEnv)
	os.Exit(finishCLI(exitCode, restartBinary, manager, restartProcess))
}

func finishCLI(exitCode int, restartBinary string, manager service.ManagerName, restart func(string, []string, []string) error) int {
	if exitCode == cli.ExitSuccess && restartBinary != "" {
		if manager == service.ManagerScheduledTask {
			return cli.ExitGeneral
		}
		if err := restart(restartBinary, os.Args, os.Environ()); err != nil {
			slog.Error("restart updated Detent process", "binary", restartBinary, "error", err)
			return cli.ExitGeneral
		}
	}
	return exitCode
}

func runCLI(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	exitCode, _ := runCLIResult(ctx, args, stdout, stderr)
	return exitCode
}

func runCLIResult(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) (int, string) {
	return runCLIResultWithRestart(ctx, args, stdout, stderr, cli.NewRestartRequest())
}

func runCLIResultWithRestart(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, restartRequest *cli.RestartRequest) (int, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	setupLoggerFromEnv(stdout, stderr)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	shutdownController := cli.NewShutdownController()
	stopSignals := notifyShutdownRequests(shutdownController, cancel, stderr, os.Exit)
	defer stopSignals()

	cmd := newRootCommandWithRestart(ctx, shutdownController, restartRequest)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)
	cmd.SilenceErrors = true
	format, formatErr := cli.ResolveOutputFormatFromArgs(args, os.Getenv, cli.WriterIsTTY(stdout))
	if err := cmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Info("shutdown requested")
			return cli.ExitSuccess, restartRequest.Binary()
		}
		return renderCommandError(cmd, err, stderr, format, formatErr), ""
	}
	return cli.ExitSuccess, restartRequest.Binary()
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
	var shutdownController *cli.ShutdownController
	if len(shutdownControllers) > 0 {
		shutdownController = shutdownControllers[0]
	}
	return newRootCommandWithRestart(ctx, shutdownController, nil)
}

func newRootCommandWithRestart(ctx context.Context, shutdownController *cli.ShutdownController, restartRequest *cli.RestartRequest) *cobra.Command {
	build := currentBuildInfo()
	opts := []cli.Option{
		cli.WithVersion(build.Version),
		cli.WithBuild(build),
		cli.WithLoggerFunc(setupLoggerFromRuntime),
		cli.WithStartupRecovery(),
	}
	if shutdownController != nil {
		opts = append(opts, cli.WithShutdownController(shutdownController))
	}
	if restartRequest != nil {
		opts = append(opts, cli.WithRestartRequest(restartRequest))
	}
	cmd := cli.NewRootCommand(ctx, opts...)
	cmd.Version = build.Version
	cmd.SetVersionTemplate("{{.Version}}\n")
	cmd.AddCommand(
		newVersionCommand(),
		newUpdateCommand(ctx, newDefaultUpdateRunner),
		newSkillCommand(build),
	)
	cli.ConfigureExamplesFirstHelp(cmd)
	cli.ConfigureCommandSuggestions(cmd)

	return cmd
}
