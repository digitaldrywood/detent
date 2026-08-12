package cli

import (
	"context"
	"fmt"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

type doctorOwnershipAttentionDiagnostic struct {
	IssueID         string `json:"issue_id,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
	State           string `json:"state,omitempty"`
	Remedy          string `json:"remedy"`
}

func checkDoctorOwnershipEligibility(
	ctx context.Context,
	projectID string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + projectID + " ownership eligibility"
	identity := cfg.Identity
	identity.Normalize()
	if !identity.Configured() || identity.OwnershipMode != workflowconfig.IdentityOwnershipAssignee {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "ownership_mode is not assignee"}
	}
	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("ownership eligibility could not be checked: %v", err), Hint: "Fix tracker credentials and rerun detent doctor."}
	}
	issues, fetchErr := projectConnector.FetchIssuesByStates(ctx, cfg.Tracker.ActiveStates)
	closeErr := closeDoctorAutoPromoteConnector(projectConnector)
	if fetchErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("active issues could not be checked for ownership eligibility: %v", fetchErr), Hint: "Fix tracker connectivity and rerun detent doctor."}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("ownership diagnostic connector could not be closed: %v", closeErr), Hint: "Rerun detent doctor and check local network resources."}
	}

	ctxSelector := selector.Context{InstanceLogin: identity.GitHubLogin, Persona: identity.Name}
	diagnostics := make([]doctorOwnershipAttentionDiagnostic, 0)
	for _, issue := range issues {
		if !doctorOwnershipAuthorized(issue, project.Authorization, cfg.Tracker.Authorization, ctxSelector) || doctorIssueHasAssignee(issue) {
			continue
		}
		diagnostics = append(diagnostics, doctorOwnershipAttentionDiagnostic{
			IssueID:         strings.TrimSpace(issue.ID),
			IssueIdentifier: strings.TrimSpace(issue.Identifier),
			IssueURL:        strings.TrimSpace(issue.URL),
			State:           strings.TrimSpace(issue.State),
			Remedy:          "assign the issue",
		})
	}
	if len(diagnostics) == 0 {
		if identity.AssigneeRequired {
			return doctorCheck{Name: name, Status: doctorOK, Detail: "all label-eligible active issues have an assignee"}
		}
		return doctorCheck{Name: name, Status: doctorOK, Detail: "ownership eligibility compatibility grace is active; no issue would be blocked by identity.assignee_required"}
	}
	if !identity.AssigneeRequired {
		return doctorCheck{
			Name:               name,
			Status:             doctorWarn,
			Detail:             fmt.Sprintf("%d label-eligible active issue(s) would stop dispatching if identity.assignee_required were enabled; compatibility grace is active and dispatch remains eligible", len(diagnostics)),
			Hint:               "Assign each reported issue before setting identity.assignee_required: true, then rerun detent doctor.",
			OwnershipAttention: diagnostics,
		}
	}

	return doctorCheck{
		Name:               name,
		Status:             doctorWarn,
		Detail:             fmt.Sprintf("%d label-eligible active issue(s) cannot dispatch under ownership_mode: assignee", len(diagnostics)),
		Hint:               "Assign each reported issue, then rerun detent doctor.",
		OwnershipAttention: diagnostics,
	}
}

func doctorOwnershipAuthorized(issue connector.Issue, projectSelector selector.Selector, workflowSelector selector.Selector, ctx selector.Context) bool {
	return (!projectSelector.Configured() || selector.Match(issue, projectSelector, ctx)) &&
		(!workflowSelector.Configured() || selector.Match(issue, workflowSelector, ctx))
}

func doctorIssueHasAssignee(issue connector.Issue) bool {
	if strings.TrimSpace(issue.AssigneeID) != "" {
		return true
	}
	for _, assignee := range issue.Assignees {
		if strings.TrimSpace(assignee) != "" {
			return true
		}
	}
	return false
}
