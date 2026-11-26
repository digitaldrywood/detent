package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (o *Orchestrator) issueContractApplies(issue connector.Issue) bool {
	if o.cfg.IssueContract == nil || issue.Metadata["hub_profile"] == "native" && strings.EqualFold(issue.State, "Triage") {
		return false
	}
	if issue.IssueContract != nil {
		return !issue.IssueContract.Exempt
	}
	at := issue.StageUpdatedAt
	if at == nil {
		at = issue.CreatedAt
	}
	return at != nil && !o.cfg.IssueContractRolloutAt.IsZero() && !at.Before(o.cfg.IssueContractRolloutAt)
}

func (o *Orchestrator) enforceIssueContract(_ context.Context, _ *State, issue connector.Issue, _ string, _ time.Time) (bool, error) {
	if strings.Contains(strings.ToLower(issue.Description), "detent:no-dispatch") {
		return false, nil
	}
	return true, nil
}

func (o *Orchestrator) filterIssueContracts(ctx context.Context, state *State, issues []connector.Issue, now time.Time) []connector.Issue {
	filtered := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		_, running := state.Running[issue.ID]
		if running || !strings.EqualFold(issue.State, firstNonBlank(o.cfg.AdmissionTargetState, "Todo")) {
			filtered = append(filtered, issue)
			continue
		}
		ok, err := o.enforceIssueContract(ctx, state, issue, issue.State, now)
		if err != nil && o.logger != nil {
			o.logger.Warn("issue contract placement failed", "issue_id", issue.ID, "error", err)
		}
		if ok && err == nil {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}
