package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/cli"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

type updateRunner interface {
	Check(context.Context) (detentupdate.Status, error)
	Apply(context.Context, detentupdate.ApplyOptions) (detentupdate.Status, error)
}

type updateFactory func(context.Context) (updateRunner, error)

func newUpdateCommand(ctx context.Context, factory updateFactory) *cobra.Command {
	var checkOnly bool
	var assumeYes bool
	var fromRelease bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Check for and apply Detent updates",
		Example:      "detent update --check",
		Args:         cli.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := cli.OutputForCommand(cmd)
			if err != nil {
				return err
			}
			if jsonOutput {
				out.Format = cli.OutputFormatJSON
			}

			runCtx := cmd.Context()
			if runCtx == nil {
				runCtx = ctx
			}
			if runCtx == nil {
				runCtx = context.Background()
			}
			runner, err := factory(runCtx)
			if err != nil {
				return err
			}

			var status detentupdate.Status
			if checkOnly {
				status, err = runner.Check(runCtx)
			} else {
				streamOut := cmd.OutOrStdout()
				if out.IsJSON() {
					streamOut = cmd.ErrOrStderr()
				}
				opts := detentupdate.ApplyOptions{
					AssumeYes:   assumeYes,
					FromRelease: fromRelease,
					Stdout:      streamOut,
					Stderr:      cmd.ErrOrStderr(),
				}
				if !assumeYes && !fromRelease && !out.IsJSON() {
					opts.Confirm = confirmUpdate(cmd)
					opts.SelectGoInstallAction = selectGoInstallAction(cmd)
				}
				status, err = runner.Apply(runCtx, opts)
			}

			if writeErr := out.Write(func(out io.Writer) error {
				return writeUpdateText(out, status)
			}, status); writeErr != nil {
				return writeErr
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "check for updates without changing the binary")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "apply the update without prompting")
	cmd.Flags().BoolVar(&fromRelease, "from-release", false, "replace a go-install-managed binary with the latest release asset")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "write machine-readable update status")
	return cmd
}

func newDefaultUpdateRunner(context.Context) (updateRunner, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve current executable: %w", err)
	}
	return detentupdate.NewService(defaultUpdateConfig(executable, currentVersionInfo(), runtime.GOOS, runtime.GOARCH)), nil
}

func defaultUpdateConfig(executable string, info versionInfo, goos string, goarch string) detentupdate.Config {
	return detentupdate.Config{
		CurrentVersion: info.Version,
		ExecutablePath: executable,
		GOOS:           goos,
		GOARCH:         goarch,
	}
}

func confirmUpdate(cmd *cobra.Command) func(detentupdate.Status) (bool, error) {
	return func(status detentupdate.Status) (bool, error) {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Update Detent from %s to %s? [y/N] ", status.CurrentVersion, status.LatestVersion); err != nil {
			return false, err
		}
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("read update confirmation: %w", err)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

func selectGoInstallAction(cmd *cobra.Command) func(detentupdate.Status) (detentupdate.GoInstallAction, error) {
	return func(status detentupdate.Status) (detentupdate.GoInstallAction, error) {
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprintf(out, "This Detent binary appears to be managed by go install.\n"); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(out, "Update Detent from %s to %s?\n", status.CurrentVersion, status.LatestVersion); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(out, "  1) Run the Go install for me: %s\n", status.Command); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintln(out, "  2) Switch to the release binary"); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(out, "     WARNING: This replaces %s and changes how Detent is managed. Future go install or go.mod upgrades will not track it.\n", status.Binary); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintln(out, "  3) Abort"); err != nil {
			return "", err
		}
		if _, err := fmt.Fprint(out, "Choose 1, 2, or 3 [3]: "); err != nil {
			return "", err
		}

		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read update choice: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "1", "g", "go", "go install", "run", "run go install":
			return detentupdate.GoInstallActionRun, nil
		case "2", "r", "release", "switch", "switch to release", "from release":
			return detentupdate.GoInstallActionRelease, nil
		case "", "3", "a", "abort", "n", "no":
			return detentupdate.GoInstallActionAbort, nil
		default:
			return "", cli.ValidationErrorf("invalid update choice: %s", strings.TrimSpace(line))
		}
	}
}

func writeUpdateText(out io.Writer, status detentupdate.Status) error {
	if strings.TrimSpace(status.Message) != "" {
		if _, err := fmt.Fprintln(out, status.Message); err != nil {
			return err
		}
	}
	if strings.TrimSpace(status.Command) != "" && (status.Action == detentupdate.ActionDelegate || status.Action == detentupdate.ActionRefused) {
		if _, err := fmt.Fprintf(out, "Run: %s\n", status.Command); err != nil {
			return err
		}
	}
	return nil
}
