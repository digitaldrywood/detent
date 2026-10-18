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

type doctorAuthorizationAttentionDiagnostic struct {
	IssueID         string        `json:"issue_id,omitempty"`
	IssueIdentifier string        `json:"issue_identifier,omitempty"`
	IssueURL        string        `json:"issue_url,omitempty"`
	State           string        `json:"state,omitempty"`
	Rule            selector.Rule `json:"rule"`
	Value           string        `json:"value,omitempty"`
	Detail          string        `json:"detail"`
}

func checkDoctorAuthorizationEligibility(
	ctx context.Context,
	projectID string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	deps doctorDeps,
) doctorCheck {
	name := "Project " + projectID + " authorization eligibility"
	if problems := doctorAuthorizationProblems(project.Authorization, cfg.Tracker.Authorization); len(problems) > 0 {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: "authorization selector is invalid: " + strings.Join(problems, "; "),
			Hint:   "Fix the authorization selector configuration, then rerun detent doctor.",
		}
	}
	authorization := doctorAuthorizationSelector(project.Authorization, cfg.Tracker.Authorization)
	if !authorization.Configured() {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "authorization selector is empty; all issues are allowed"}
	}
	if deps.autoPromoteConnector == nil {
		deps.autoPromoteConnector = defaultDoctorAutoPromoteConnector
	}
	projectConnector, err := deps.autoPromoteConnector(cfg)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("authorization eligibility could not be checked: %v", err), Hint: "Fix tracker credentials and rerun detent doctor."}
	}
	if projectConnector == nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: "authorization eligibility could not be checked: connector is nil", Hint: "Fix tracker configuration and rerun detent doctor."}
	}
	states := doctorRequiredGitHubStatusStates(cfg)
	issues, fetchErr := projectConnector.FetchIssuesByStates(ctx, states)
	closeErr := closeDoctorAutoPromoteConnector(projectConnector)
	if fetchErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("lane-labeled issues could not be checked for authorization eligibility: %v", fetchErr), Hint: "Fix tracker connectivity and rerun detent doctor."}
	}
	if closeErr != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("authorization diagnostic connector could not be closed: %v", closeErr), Hint: "Rerun detent doctor and check local network resources."}
	}

	identity := cfg.Identity
	identity.Normalize()
	selectorContext := selector.Context{InstanceLogin: identity.GitHubLogin, Persona: identity.Name}
	diagnostics := make([]doctorAuthorizationAttentionDiagnostic, 0)
	for _, issue := range issues {
		if issue.Closed || !doctorIssueHasLaneLabel(issue, cfg.Tracker.StatusLabelPrefix) {
			continue
		}
		decision := selector.Decide(issue, authorization, selectorContext)
		if decision.Matched {
			continue
		}
		diagnostics = append(diagnostics, doctorAuthorizationAttentionDiagnostic{
			IssueID:         strings.TrimSpace(issue.ID),
			IssueIdentifier: strings.TrimSpace(issue.Identifier),
			IssueURL:        strings.TrimSpace(issue.URL),
			State:           strings.TrimSpace(issue.State),
			Rule:            decision.Rule,
			Value:           decision.Value,
			Detail:          decision.Detail,
		})
	}
	if len(diagnostics) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "all open lane-labeled issues match the authorization selector"}
	}
	summaries := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		identifier := diagnostic.IssueIdentifier
		if identifier == "" {
			identifier = diagnostic.IssueID
		}
		summaries = append(summaries, identifier+": "+diagnostic.Detail)
	}
	return doctorCheck{
		Name:                   name,
		Status:                 doctorWarn,
		Detail:                 fmt.Sprintf("%d open lane-labeled issue(s) cannot dispatch: %s", len(diagnostics), strings.Join(summaries, "; ")),
		Hint:                   "Correct each issue label or author to match the authorization selector, or update the selector intentionally.",
		AuthorizationAttention: diagnostics,
	}
}

func doctorAuthorizationProblems(projectSelector selector.Selector, workflowSelector selector.Selector) []string {
	problems := projectSelector.Validate("global.authorization")
	return append(problems, workflowSelector.Validate("tracker.authorization")...)
}

func doctorAuthorizationSelector(selectors ...selector.Selector) selector.Selector {
	configured := make([]selector.Selector, 0, len(selectors))
	for _, candidate := range selectors {
		if candidate.Configured() {
			configured = append(configured, candidate)
		}
	}
	switch len(configured) {
	case 0:
		return selector.Selector{}
	case 1:
		return configured[0]
	default:
		return selector.Selector{And: configured}
	}
}

func doctorIssueHasLaneLabel(issue connector.Issue, prefix string) bool {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return false
	}
	for _, label := range issue.Labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if strings.HasPrefix(label, prefix) && strings.TrimSpace(strings.TrimPrefix(label, prefix)) != "" {
			return true
		}
	}
	return false
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
