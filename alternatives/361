package doctor

import (
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func checkDisabledBudgetCaps(id string, cfg workflowconfig.Budget) (Check, bool) {
	if cfg.Enabled {
		return Check{}, false
	}

	caps := make([]string, 0, 2)
	if cfg.PerDayMaxUSDConfigured() && cfg.PerDayMaxUSD > 0 {
		caps = append(caps, fmt.Sprintf("budget.per_day_max_usd=%g", cfg.PerDayMaxUSD))
	}
	if cfg.PerIssueMaxUSDConfigured() && cfg.PerIssueMaxUSD > 0 {
		caps = append(caps, fmt.Sprintf("budget.per_issue_max_usd=%g", cfg.PerIssueMaxUSD))
	}
	if len(caps) == 0 {
		return Check{}, false
	}

	return Check{
		Name:   "Project " + id + " budget",
		Status: Warn,
		Detail: "budget.enabled=false disables configured caps: " + strings.Join(caps, ", "),
		Hint:   "Add this exact line under budget: in WORKFLOW.md:\n  enabled: true",
	}, true
}

func ProjectID(project globalconfig.Project) string {
	id := strings.TrimSpace(project.ID)
	if id == "" {
		return "project"
	}
	return id
}

func ProjectSourceRoot(project globalconfig.Project, cfg workflowconfig.Config) string {
	if sourceRoot := strings.TrimSpace(cfg.Workspace.SourceRoot); sourceRoot != "" {
		return sourceRoot
	}
	if workdir := strings.TrimSpace(project.Workdir); workdir != "" {
		return workdir
	}
	if root := strings.TrimSpace(cfg.Workspace.Root); root != "" {
		return root
	}
	return ""
}
