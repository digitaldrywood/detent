package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

type onboardingMergePolicyInspectionConfig struct {
	SourceRoot string
	Repository string
}

type onboardingMergePolicyInspectionResult struct {
	Repository               string `json:"repository"`
	SelectedMergeMethod      string `json:"selected_merge_method"`
	SelectedMethodEnabled    bool   `json:"selected_method_enabled"`
	AdditionalMethodsEnabled bool   `json:"additional_methods_enabled"`
	AllowMergeCommit         bool   `json:"allow_merge_commit"`
	AllowSquashMerge         bool   `json:"allow_squash_merge"`
	AllowRebaseMerge         bool   `json:"allow_rebase_merge"`
	SelectionSource          string `json:"selection_source"`
}

type onboardingMergePolicyInspectionDeps struct {
	loadWorkflow        func(string) (workflowconfig.Workflow, error)
	githubMergeSettings func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryMergeSettings, error)
}

func newOnboardingInspectMergePolicyCommand() *cobra.Command {
	var sourceRoot string
	var repository string
	cmd := &cobra.Command{
		Use:          "inspect-merge-policy",
		Short:        "Inspect the effective onboarding merge policy and GitHub settings",
		Example:      `detent --format json onboarding inspect-merge-policy --source-root /path/to/repo --repository owner/repo`,
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			result, err := inspectOnboardingMergePolicy(cmd.Context(), onboardingMergePolicyInspectionConfig{
				SourceRoot: sourceRoot,
				Repository: repository,
			}, onboardingMergePolicyInspectionDeps{})
			if err != nil {
				return err
			}
			return out.Write(func(w io.Writer) error {
				return writeOnboardingMergePolicyInspectionPretty(w, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&sourceRoot, "source-root", ".", "target repository checkout root")
	cmd.Flags().StringVar(&repository, "repository", "", "target GitHub repository in owner/name form")
	return cmd
}

func inspectOnboardingMergePolicy(
	ctx context.Context,
	cfg onboardingMergePolicyInspectionConfig,
	deps onboardingMergePolicyInspectionDeps,
) (onboardingMergePolicyInspectionResult, error) {
	repository := strings.TrimSpace(cfg.Repository)
	if !onboardingAnswerRepositoryPattern.MatchString(repository) {
		return onboardingMergePolicyInspectionResult{}, NewValidationError(
			"--repository must look like owner/name",
			"Pass the confirmed TARGET_REPOSITORY value from answers.env.",
			nil,
		)
	}
	sourceRoot, err := resolveOnboardingGateSourceRoot(cfg.SourceRoot)
	if err != nil {
		return onboardingMergePolicyInspectionResult{}, err
	}
	if deps.loadWorkflow == nil {
		deps.loadWorkflow = workflowconfig.LoadWorkflow
	}
	if deps.githubMergeSettings == nil {
		deps.githubMergeSettings = defaultDoctorGitHubMergeSettings
	}

	workflowPath := filepath.Join(sourceRoot, defaultWorkflowFile)
	effective := workflowconfig.Default()
	selectionSource := "template_default"
	present, err := onboardingProjectDefinitionPresent(workflowPath)
	if err != nil {
		return onboardingMergePolicyInspectionResult{}, err
	}
	if present {
		workflow, err := deps.loadWorkflow(workflowPath)
		if err != nil {
			return onboardingMergePolicyInspectionResult{}, fmt.Errorf("load effective project definition: %w", err)
		}
		effective = workflow.Config
		selectionSource = "effective_project_definition"
	}

	settings, err := deps.githubMergeSettings(ctx, effective, repository)
	if err != nil {
		return onboardingMergePolicyInspectionResult{}, fmt.Errorf("read %s merge settings: %w", repository, err)
	}
	selected := effective.Deliverable.EffectiveMergeMethod()
	return onboardingMergePolicyInspectionResult{
		Repository:               repository,
		SelectedMergeMethod:      selected,
		SelectedMethodEnabled:    doctorRepositoryMergeMethodEnabled(settings, selected),
		AdditionalMethodsEnabled: onboardingAdditionalMergeMethodsEnabled(settings, selected),
		AllowMergeCommit:         settings.AllowMergeCommit,
		AllowSquashMerge:         settings.AllowSquashMerge,
		AllowRebaseMerge:         settings.AllowRebaseMerge,
		SelectionSource:          selectionSource,
	}, nil
}

func onboardingProjectDefinitionPresent(workflowPath string) (bool, error) {
	for _, path := range []string{
		workflowPath,
		workflowconfig.DefinitionPath(workflowPath),
		workflowconfig.LocalWorkflowPath(workflowPath),
		workflowconfig.LocalDefinitionPath(workflowPath),
	} {
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect project definition %s: %w", path, err)
		}
	}
	return false, nil
}

func onboardingAdditionalMergeMethodsEnabled(settings ghconnector.RepositoryMergeSettings, selected string) bool {
	return selected != workflowconfig.MergeMethodMerge && settings.AllowMergeCommit ||
		selected != workflowconfig.MergeMethodSquash && settings.AllowSquashMerge ||
		selected != workflowconfig.MergeMethodRebase && settings.AllowRebaseMerge
}

func writeOnboardingMergePolicyInspectionPretty(w io.Writer, result onboardingMergePolicyInspectionResult) error {
	_, err := fmt.Fprintf(
		w,
		"repository: %s\nselected_merge_method: %s\nselection_source: %s\nselected_method_enabled: %t\nadditional_methods_enabled: %t\nallow_merge_commit: %t\nallow_squash_merge: %t\nallow_rebase_merge: %t\n",
		result.Repository,
		result.SelectedMergeMethod,
		result.SelectionSource,
		result.SelectedMethodEnabled,
		result.AdditionalMethodsEnabled,
		result.AllowMergeCommit,
		result.AllowSquashMerge,
		result.AllowRebaseMerge,
	)
	return err
}
