package cli

import (
	"context"
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

const doctorWorkflowRuleRepositoryMergePolicy = "repository_merge_policy"

func defaultDoctorGitHubMergeSettings(ctx context.Context, cfg workflowconfig.Config, repository string) (ghconnector.RepositoryMergeSettings, error) {
	connector, err := ghconnector.NewConnector(doctorGitHubConnectorConfig(cfg))
	if err != nil {
		return ghconnector.RepositoryMergeSettings{}, err
	}
	return connector.RepositoryMergeSettings(ctx, repository)
}

func checkDoctorRepositoryMergePolicy(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	name := "Project " + id + " repository merge policy"
	if deps.githubMergeSettings == nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "repository merge policy skipped because the GitHub settings reader is unavailable"}
	}
	repositories := doctorGitHubRepositories(ctx, project, cfg, deps, projectSourceRoot(project, cfg))
	if len(repositories) == 0 {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "repository merge policy skipped because no GitHub repository could be resolved", Hint: "Set tracker.repository to owner/repo in WORKFLOW.md."}
	}

	warnings := []string{}
	findings := []doctorWorkflowOptimizationFinding{}
	for _, repository := range repositories {
		settings, err := deps.githubMergeSettings(ctx, cfg, repository)
		if err != nil {
			return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("read %s merge settings: %v", repository, err), Hint: "Fix GitHub repository access, then rerun detent doctor."}
		}
		detail, fix, patch := doctorRepositoryMergePolicyWarning(repository, cfg.Deliverable, settings)
		if detail == "" {
			continue
		}
		warnings = append(warnings, detail+"; fix: "+fix)
		finding := doctorWorkflowOptimizationFinding{
			RuleID:    doctorWorkflowRuleRepositoryMergePolicy,
			ProjectID: id,
			Severity:  "warning",
			Title:     "repository merge policy drift",
			Detail:    detail,
			Evidence: map[string]any{
				"repository": repository,
				"settings":   doctorRepositoryMergeSettingsDetail(settings),
				"fix":        fix,
			},
		}
		if patch != nil {
			finding.Patch = []doctorWorkflowOptimizationPatch{*patch}
		}
		findings = append(findings, finding)
	}
	if len(warnings) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "repository merge settings exactly match the declared strategy"}
	}
	report := doctorWorkflowOptimizationReport{Findings: findings, Proposals: doctorWorkflowProposalsForFindings(id, findings, 1)}
	return doctorCheck{Name: name, Status: doctorWarn, Detail: strings.Join(warnings, "; "), Hint: "Apply each copy-paste fix, then rerun detent doctor.", WorkflowOptimization: report}
}

func doctorRepositoryMergePolicyWarning(repository string, deliverable workflowconfig.Deliverable, settings ghconnector.RepositoryMergeSettings) (string, string, *doctorWorkflowOptimizationPatch) {
	settingsDetail := doctorRepositoryMergeSettingsDetail(settings)
	if !deliverable.MergeMethodConfigured() {
		if doctorRepositoryEnabledMergeMethodCount(settings) <= 1 {
			return "", "", nil
		}
		patch := doctorWorkflowOptimizationPatch{Path: "deliverable.merge_method", Value: workflowconfig.MergeMethodSquash}
		return fmt.Sprintf("%s ambiguous merge policy: repo settings %s allow multiple methods and WORKFLOW.md declares none; agent-side auto-detection can produce mixed history", repository, settingsDetail), "add `merge_method: squash` under `deliverable:` in WORKFLOW.md", &patch
	}

	declared := deliverable.EffectiveMergeMethod()
	if !doctorRepositoryMergeMethodEnabled(settings, declared) {
		return fmt.Sprintf("%s declared merge_method=%s is forbidden by repo settings %s", repository, declared, settingsDetail), doctorRepositoryMergeSettingsCommand(repository, declared), nil
	}
	if doctorRepositoryEnabledMergeMethodCount(settings) > 1 {
		return fmt.Sprintf("%s declared merge_method=%s is loose because repo settings %s permit additional methods", repository, declared, settingsDetail), doctorRepositoryMergeSettingsCommand(repository, declared), nil
	}
	return "", "", nil
}

func doctorRepositoryMergeSettingsDetail(settings ghconnector.RepositoryMergeSettings) string {
	return fmt.Sprintf("merge_commit=%t,squash=%t,rebase=%t", settings.AllowMergeCommit, settings.AllowSquashMerge, settings.AllowRebaseMerge)
}

func doctorRepositoryEnabledMergeMethodCount(settings ghconnector.RepositoryMergeSettings) int {
	count := 0
	for _, enabled := range []bool{settings.AllowMergeCommit, settings.AllowSquashMerge, settings.AllowRebaseMerge} {
		if enabled {
			count++
		}
	}
	return count
}

func doctorRepositoryMergeMethodEnabled(settings ghconnector.RepositoryMergeSettings, method string) bool {
	switch method {
	case workflowconfig.MergeMethodMerge:
		return settings.AllowMergeCommit
	case workflowconfig.MergeMethodSquash:
		return settings.AllowSquashMerge
	case workflowconfig.MergeMethodRebase:
		return settings.AllowRebaseMerge
	default:
		return false
	}
}

func doctorRepositoryMergeSettingsCommand(repository string, method string) string {
	return fmt.Sprintf("gh api --method PATCH repos/%s -F allow_merge_commit=%t -F allow_squash_merge=%t -F allow_rebase_merge=%t", repository, method == workflowconfig.MergeMethodMerge, method == workflowconfig.MergeMethodSquash, method == workflowconfig.MergeMethodRebase)
}
