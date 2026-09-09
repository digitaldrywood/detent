package orchestrator

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const mergeControlCheckedHeadOutput = "merge_control_checked_head"

func (p dispatchPlanner) readyMergeControlCandidate(state *State, issue connector.Issue) bool {
	if !p.mechanicalMergeAdmission(issue) || !mergeWorkerProgrammaticMergeReady(issue) || strings.TrimSpace(issue.PullRequest.BaseSHA) == "" {
		return false
	}
	if retry, ok := state.Retry[issue.ID]; ok && retry.MergePrecheck != nil {
		return false
	}
	return state.mergeReservations[mergeWorkerRepositoryKey(issue)].RefreshHeadSHA != issue.PullRequest.HeadSHA
}

func sameMergeControlRevision(checked, current connector.Issue) bool {
	return checked.PullRequest != nil && current.PullRequest != nil &&
		pullRequestRepository(checked) == pullRequestRepository(current) &&
		pullRequestNumber(checked) == pullRequestNumber(current) &&
		checked.PullRequest.HeadSHA == current.PullRequest.HeadSHA &&
		checked.PullRequest.BaseSHA == current.PullRequest.BaseSHA
}

func (o *Orchestrator) reconcileMergeControlDemand(decisions []dispatchPlanDecision, outcomes map[string]dispatchIssueOutcome) {
	completedControl := false
	for _, outcome := range outcomes {
		if outcome.reason == dispatchIssueFailureGlobalSlotUnavailable {
			return
		}
		completedControl = completedControl || outcome.mergeControl
	}
	if !completedControl {
		return
	}
	for _, decision := range decisions {
		if decision.SkipReason == dispatchSkipMergeControlLimit {
			return
		}
	}
	o.markGlobalProjectIdle()
}
