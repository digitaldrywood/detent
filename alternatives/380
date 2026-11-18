package cli

import (
	"context"
	"fmt"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func checkDoctorBranchPolicy(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) []doctorCheck {
	deps = deps.withDefaults()
	var checks []doctorCheck
	if doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) && cfg.Deliverable.Kind == workflowconfig.DeliverablePullRequest {
		for _, repository := range doctorGitHubRepositories(ctx, project, cfg, deps, projectSourceRoot(project, cfg)) {
			policy, err := deps.githubBranchPolicy(ctx, cfg, repository)
			check := doctorCheck{Name: "Project " + id + " branch policy", Status: doctorOK}
			switch {
			case err != nil:
				check.Status = doctorWarn
				check.Detail = repository + ": branch policy could not be read: " + err.Error()
			case policy.RulesUnavailableOnPlan:
				check.Detail = repository + ": branch rules are not available on this plan"
			default:
				check.Detail = fmt.Sprintf("%s branch %s requires up-to-date branches: %t", repository, policy.Branch, policy.Strict)
			}
			checks = append(checks, check)
		}
	}
	return checks
}
