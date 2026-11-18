package dispatchplan

import (
	"slices"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
)

type EligibilityConfig struct {
	ActiveStates     []string
	TerminalStates   []string
	Authorization    selector.Selector
	SelectorContext  selector.Context
	OwnershipSet     bool
	OwnershipMode    string
	AssigneeRequired bool
}

type Eligibility struct {
	cfg        EligibilityConfig
	cloneIssue func(connector.Issue) connector.Issue
}

func NewEligibility(cfg EligibilityConfig, cloneIssue func(connector.Issue) connector.Issue) Eligibility {
	return Eligibility{cfg: cfg, cloneIssue: cloneIssue}
}

func (p Eligibility) Authorized(issue connector.Issue) bool {
	if !p.cfg.Authorization.Configured() {
		return true
	}
	return selector.Match(issue, p.cfg.Authorization, p.cfg.SelectorContext)
}

func (p Eligibility) AuthorizationDecision(issue connector.Issue) selector.Decision {
	return selector.Decide(issue, p.cfg.Authorization, p.cfg.SelectorContext)
}

func (p Eligibility) NeedsAssignee(issue connector.Issue) bool {
	return p.cfg.AssigneeRequired && p.AssigneeEligibilityApplies(issue)
}

func (p Eligibility) AssigneeEligibilityApplies(issue connector.Issue) bool {
	if !p.cfg.OwnershipSet || p.cfg.OwnershipMode != workflowconfig.IdentityOwnershipAssignee {
		return false
	}
	if strings.TrimSpace(issue.AssigneeID) != "" {
		return false
	}
	for _, assignee := range issue.Assignees {
		if strings.TrimSpace(assignee) != "" {
			return false
		}
	}
	return true
}

func (p Eligibility) AssigneeEligibilityCandidates(issues []connector.Issue) []connector.Issue {
	candidates := make([]connector.Issue, 0)
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" || !ValidCandidate(issue) || !stateIn(issue.State, p.cfg.ActiveStates) || stateIn(issue.State, p.cfg.TerminalStates) || !p.Authorized(issue) || !p.AssigneeEligibilityApplies(issue) {
			continue
		}
		candidates = append(candidates, p.cloneIssue(issue))
	}
	return candidates
}

func ValidCandidate(issue connector.Issue) bool {
	return issue.ID != "" &&
		issue.Identifier != "" &&
		issue.Title != "" &&
		issue.State != "" &&
		!issue.Closed &&
		issue.AssignedToWorker
}

func LabelSelector(auth selector.Selector) selector.Selector {
	labels := selector.Selector{Labels: auth.Labels}
	for _, child := range auth.And {
		labels.And = append(labels.And, LabelSelector(child))
	}
	for _, child := range auth.Or {
		labels.Or = append(labels.Or, LabelSelector(child))
	}
	return labels
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func stateIn(state string, states []string) bool {
	return slices.Contains(states, normalizeState(state))
}
