package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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

func newFixCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "fix",
		Short:   "Repair Detent project configuration",
		Example: "detent fix workflow-layout --workflow /repo/WORKFLOW.md --dry-run",
	}
	cmd.AddCommand(newWorkflowLayoutFixCommand())
	return cmd
}

func newWorkflowLayoutFixCommand() *cobra.Command {
	var workflowPath string
	var dryRun bool
	var confirmed bool
	cmd := &cobra.Command{
		Use:     "workflow-layout",
		Short:   "Migrate legacy workflow configuration into detent.yaml",
		Example: "detent fix workflow-layout --workflow /repo/WORKFLOW.md --dry-run\n  detent fix workflow-layout --workflow /repo/WORKFLOW.md --yes",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(workflowPath) == "" {
				return WrapValidation(errors.New("--workflow is required"))
			}
			if dryRun && confirmed {
				return WrapValidation(errors.New("--dry-run and --yes cannot be used together"))
			}
			plan, err := workflowconfig.PlanProjectDefinitionMigration(workflowPath)
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
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), "Apply this workflow-layout migration? [y/N] "); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read migration confirmation: %w", err)
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
