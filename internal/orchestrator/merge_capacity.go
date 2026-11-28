package orchestrator

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const mergeControlCheckedHeadReason = "merge_control_checked_head"

func (p dispatchPlanner) allowsDirectPullRequestMerge() bool {
	return !p.nativeWorkflow && !p.boundedAdmission && !p.cfg.Claiming.Enabled
}

func (p dispatchPlanner) readyMergeControlCandidate(state *State, issue connector.Issue) bool {
	if !p.allowsDirectPullRequestMerge() || !p.mechanicalMergeAdmission(issue) || !mergeWorkerProgrammaticMergeReady(issue, p.cfg) || issue.PullRequest.BaseBranchStrict || strings.TrimSpace(issue.PullRequest.BaseSHA) == "" {
		return false
	}
	if retry, ok := state.Retry[issue.ID]; ok && retry.MergePrecheck != nil {
		return false
	}
	return state.mergeReservations[issue.ID].RefreshHeadSHA != issue.PullRequest.HeadSHA
}

func sameMergeRevision(checked, current connector.Issue) bool {
	repository := pullRequestRepository(checked)
	number := pullRequestNumber(checked)
	return checked.PullRequest != nil && current.PullRequest != nil &&
		repository != "" && repository == pullRequestRepository(current) &&
		number > 0 && number == pullRequestNumber(current) &&
		checked.PullRequest.HeadSHA != "" && checked.PullRequest.HeadSHA == current.PullRequest.HeadSHA
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
