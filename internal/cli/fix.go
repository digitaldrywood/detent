package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type workflowLayoutFixResult struct {
	WorkflowPath    string                                               `json:"workflow_path"`
	BeforeLayout    workflowconfig.ProjectDefinitionLayout               `json:"before_layout"`
	AfterLayout     workflowconfig.ProjectDefinitionLayout               `json:"after_layout"`
	LegacyKeys      []string                                             `json:"legacy_keys,omitempty"`
	LocalLegacyKeys []string                                             `json:"local_legacy_keys,omitempty"`
	Operations      []workflowconfig.ProjectDefinitionMigrationOperation `json:"operations"`
	SemanticDiff    string                                               `json:"semantic_diff"`
	DryRun          bool                                                 `json:"dry_run"`
	Applied         bool                                                 `json:"applied"`
	Cancelled       bool                                                 `json:"cancelled"`
	Noop            bool                                                 `json:"noop"`
}

func newFixCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "fix",
		Short:   "Repair Detent project configuration",
		Example: "detent fix workflow-layout --workflow /repo/WORKFLOW.md --dry-run\n  detent fix agent-pools --dry-run",
	}
	cmd.AddCommand(
		newWorkflowLayoutFixCommand(configPath, opts),
		newAgentPoolsFixCommand(configPath, opts),
		newWorkerProcessesFixCommand(configPath, opts),
	)
	return cmd
}

func newWorkflowLayoutFixCommand(configPath *string, opts options) *cobra.Command {
	var workflowPath string
	var dryRun bool
	var confirmed bool
	cmd := &cobra.Command{
		Use:     "workflow-layout",
		Short:   "Migrate legacy workflow configuration into detent.yaml",
		Long:    "Migrate legacy workflow configuration into detent.yaml. GitHub credentials resolve from GITHUB_TOKEN or the resolved global config, including github_token: gh.",
		Example: "detent fix workflow-layout --workflow /repo/WORKFLOW.md --dry-run\n  detent fix workflow-layout --workflow /repo/WORKFLOW.md --yes",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(workflowPath) == "" {
				return WrapValidation(errors.New("--workflow is required"))
			}
			if dryRun && confirmed {
				return WrapValidation(errors.New("--dry-run and --yes cannot be used together"))
			}
			plan, err := workflowconfig.PlanProjectDefinitionMigration(
				workflowPath,
				workflowconfig.WithProjectDefinitionMigrationGitHubTokenResolver(func(cfg workflowconfig.Config) (string, error) {
					return resolveWorkflowLayoutMigrationGitHubToken(cmd.Context(), cfg, derefString(configPath), opts)
				}),
			)
			if err != nil {
				return err
			}
			result := newWorkflowLayoutFixResult(plan)
			result.DryRun = dryRun
			result.Noop = plan.Noop
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			if dryRun || plan.Noop {
				return out.Write(func(writer io.Writer) error {
					return writeWorkflowLayoutFixResult(writer, result)
				}, result)
			}

			if !confirmed {
				if !out.IsJSON() {
					if err := writeWorkflowLayoutFixResult(cmd.OutOrStdout(), result); err != nil {
						return err
					}
				}
				ok, err := confirmWorkflowLayoutFix(cmd)
				if err != nil {
					return err
				}
				if !ok {
					result.Cancelled = true
					if out.IsJSON() {
						return out.Write(nil, result)
					}
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "Migration cancelled; no files changed.")
					return err
				}
			}

			if err := workflowconfig.ApplyProjectDefinitionMigration(plan); err != nil {
				return err
			}
			result.Applied = true
			return out.Write(func(writer io.Writer) error {
				return writeWorkflowLayoutFixResult(writer, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&workflowPath, "workflow", "", "path to the WORKFLOW.md project-definition anchor")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the migration without writing files")
	cmd.Flags().BoolVar(&confirmed, "yes", false, "apply the migration without an interactive confirmation")
	return cmd
}

func resolveWorkflowLayoutMigrationGitHubToken(
	ctx context.Context,
	effective workflowconfig.Config,
	configPath string,
	opts options,
) (string, error) {
	deps := runtimeDepsFromOptions(opts).withDefaults()
	if !trackerUsesGitHubToken(effective.Tracker.Kind) {
		return "", nil
	}
	if trackerHasGitHubAppCredentials(effective.Tracker, deps.lookupEnv) {
		return "", nil
	}
	if token, _ := resolveRuntimeSecret(effective.Tracker.APIKey, deps.lookupEnv); token != "" {
		return "", nil
	}

	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return "", err
	}
	read := opts.read
	if read == nil {
		read = func(path string) (globalconfig.Config, error) {
			return globalconfig.Read(path)
		}
	}
	global, err := read(resolution.Path)
	if err != nil {
		var missing globalconfig.MissingFileError
		if !errors.As(err, &missing) || !errors.Is(missing.Err, os.ErrNotExist) {
			return "", err
		}
		global = globalconfig.Config{}
	}
	global.Projects = []globalconfig.Project{{Workflow: "workflow-layout migration"}}
	deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
		return workflowconfig.Workflow{Config: effective}, nil
	}
	token, _, err := resolveRuntimeGitHubToken(ctx, &global, deps)
	if err != nil {
		return "", err
	}
	return runtimeGlobalGitHubToken(token), nil
}

func newWorkflowLayoutFixResult(plan workflowconfig.ProjectDefinitionMigrationPlan) workflowLayoutFixResult {
	return workflowLayoutFixResult{
		WorkflowPath:    plan.WorkflowPath,
		BeforeLayout:    plan.BeforeLayout,
		AfterLayout:     plan.AfterLayout,
		LegacyKeys:      append([]string(nil), plan.LegacyKeys...),
		LocalLegacyKeys: append([]string(nil), plan.LocalLegacyKeys...),
		Operations:      append([]workflowconfig.ProjectDefinitionMigrationOperation(nil), plan.Operations...),
		SemanticDiff:    plan.SemanticDiff,
	}
}

func confirmWorkflowLayoutFix(cmd *cobra.Command) (bool, error) {
	return confirmFix(cmd, "Apply this workflow-layout migration?", "migration")
}

func confirmFix(cmd *cobra.Command, prompt string, action string) (bool, error) {
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", prompt); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read %s confirmation: %w", action, err)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func writeWorkflowLayoutFixResult(out io.Writer, result workflowLayoutFixResult) error {
	if _, err := fmt.Fprintf(out, "Workflow layout: %s -> %s\n", result.BeforeLayout, result.AfterLayout); err != nil {
		return err
	}
	if len(result.LegacyKeys) > 0 {
		if _, err := fmt.Fprintln(out, "Legacy keys:", strings.Join(result.LegacyKeys, ", ")); err != nil {
			return err
		}
	}
	if len(result.LocalLegacyKeys) > 0 {
		if _, err := fmt.Fprintln(out, "Local legacy keys:", strings.Join(result.LocalLegacyKeys, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "Proposed file operations:"); err != nil {
		return err
	}
	if len(result.Operations) == 0 {
		if _, err := fmt.Fprintln(out, "  none"); err != nil {
			return err
		}
	}
	for _, operation := range result.Operations {
		if _, err := fmt.Fprintf(out, "  %s %s (mode %04o)\n", operation.Action, operation.Path, operation.Mode.Perm()); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "Semantic diff:", result.SemanticDiff); err != nil {
		return err
	}
	switch {
	case result.Noop:
		_, err := fmt.Fprintln(out, "No changes needed.")
		return err
	case result.DryRun:
		_, err := fmt.Fprintln(out, "Dry run; no files changed.")
		return err
	case result.Applied:
		_, err := fmt.Fprintln(out, "Migration applied.")
		return err
	default:
		return nil
	}
}
