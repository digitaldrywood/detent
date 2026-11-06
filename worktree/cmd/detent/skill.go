package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cli"
	"github.com/digitaldrywood/detent/internal/skillinstall"
)

type skillInstallFunc func(skillinstall.Config) (skillinstall.Result, error)

type skillInstallDeps struct {
	homeDir func() (string, error)
	install skillInstallFunc
	build   buildinfo.Info
}

func newSkillCommand(build buildinfo.Info) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "skill",
		Short:   "Manage Detent agent skills",
		Example: "detent skill install --target codex --dry-run",
	}
	cmd.AddCommand(newSkillInstallCommand(skillInstallDeps{
		homeDir: os.UserHomeDir,
		install: skillinstall.Install,
		build:   build,
	}))
	return cmd
}

func newSkillInstallCommand(deps skillInstallDeps) *cobra.Command {
	var targetValues []string
	var dryRun bool
	var force bool

	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install the embedded operator skill",
		Example:      "detent skill install --target claude-code --target codex --dry-run",
		Args:         cli.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(targetValues) == 0 {
				return cli.NewValidationError("skill install --target is required", "Pass --target claude-code, --target codex, or --target antigravity.", nil)
			}
			targets, err := skillinstall.ParseTargets(targetValues)
			if err != nil {
				return cli.NewValidationError(err.Error(), "Use only claude-code, codex, or antigravity as target values.", nil)
			}
			if deps.homeDir == nil || deps.install == nil {
				return errors.New("skill installer dependencies are incomplete")
			}
			home, err := deps.homeDir()
			if err != nil {
				return fmt.Errorf("resolve user home: %w", err)
			}
			roots, err := skillinstall.Roots(home)
			if err != nil {
				return cli.NewValidationError(err.Error(), "Use an absolute user home directory without symlink escapes.", nil)
			}
			out, err := cli.OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, installErr := deps.install(skillinstall.Config{
				Roots:   roots,
				Targets: targets,
				Build:   deps.build,
				DryRun:  dryRun,
				Force:   force,
			})
			writeErr := out.Write(func(writer io.Writer) error {
				return writeSkillInstallText(writer, result)
			}, result)
			if writeErr != nil {
				return errors.Join(installErr, writeErr)
			}
			if installErr == nil {
				return nil
			}
			switch {
			case errors.Is(installErr, skillinstall.ErrConflict):
				return cli.NewValidationError(
					installErr.Error(),
					"Review the reported differences, then rerun with --force to create deterministic backups and replace managed files.",
					nil,
				)
			case errors.Is(installErr, skillinstall.ErrUnsafePath):
				return cli.NewValidationError(installErr.Error(), "No files were changed. Review the reported unsafe path and retry.", nil)
			case result.Status == "rollback_failed":
				return &cli.HintedError{
					Err:     installErr,
					Message: installErr.Error(),
					Hint:    "Rollback did not restore every path. Inspect the reported rollback failures and repair the listed paths before retrying.",
				}
			default:
				return &cli.HintedError{
					Err:     installErr,
					Message: installErr.Error(),
					Hint:    "The installer rolled back completed actions. Review the operational error and retry when the filesystem is healthy.",
				}
			}
		},
	}
	cmd.Flags().StringSliceVar(&targetValues, "target", nil, "agent client target: claude-code, codex, or antigravity (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan and validate every action without writing files")
	cmd.Flags().BoolVar(&force, "force", false, "back up and replace differing managed files")
	return cmd
}

func writeSkillInstallText(writer io.Writer, result skillinstall.Result) error {
	if _, err := fmt.Fprintf(
		writer,
		"skill: %s bundle=%d detent=%s status=%s\n",
		result.Bundle.Name,
		result.Bundle.Version,
		result.Build.Version,
		result.Status,
	); err != nil {
		return err
	}
	for _, target := range result.Targets {
		if _, err := fmt.Fprintf(writer, "target: %s intent=%s status=%s destination=%s\n", target.Target, target.Intent, target.Status, target.Destination); err != nil {
			return err
		}
	}
	for _, action := range result.Actions {
		path := action.Path
		if action.BackupPath != "" {
			path += " -> " + action.BackupPath
		}
		if _, err := fmt.Fprintf(writer, "action: %s %s %s [%s]\n", action.Target, action.Action, path, action.Status); err != nil {
			return err
		}
	}
	for _, action := range result.Rollback {
		if _, err := fmt.Fprintf(writer, "rollback: %s %s %s [%s]\n", action.Target, action.Action, action.Path, action.Status); err != nil {
			return err
		}
	}
	return nil
}
