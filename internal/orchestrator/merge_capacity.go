package orchestrator

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const mergeControlCheckedHeadReason = "merge_control_checked_head"
const nativeMergeRequired = "landing requires a native Change or a forge that can enforce the approved base"

func (p dispatchPlanner) allowsLegacyMergeCompletion() bool {
	return !p.nativeWorkflow && !p.boundedAdmission && !p.cfg.Claiming.Enabled
}

func (p dispatchPlanner) allowsDirectPullRequestMerge() bool {
	return !p.headOnlyMerge && p.allowsLegacyMergeCompletion() && !p.durableWorkAttempts
}

func (o *Orchestrator) allowsLegacyMergeLaneWrite(running Running, requestAttemptID int64) bool {
	p := o.dispatchPlanner()
	return p.allowsLegacyMergeCompletion() && !p.durableWorkAttempts && running.WorkAttemptID == 0 && requestAttemptID == 0
}

func (p dispatchPlanner) readyMergeControlCandidate(state *State, issue connector.Issue) bool {
	if !p.allowsDirectPullRequestMerge() || !p.mechanicalMergeAdmission(issue) || !mergeWorkerProgrammaticMergeReady(issue, p.cfg) || issue.PullRequest.BaseBranchStrict || strings.TrimSpace(issue.PullRequest.BaseSHA) == "" || strings.TrimSpace(issue.PullRequest.BaseRef) == "" {
		return false
	}
	if retry, ok := state.Retry[issue.ID]; ok && retry.MergePrecheck != nil {
		return false
	}
	return state.mergeReservations[issue.ID].RefreshHeadSHA != issue.PullRequest.HeadSHA
}

func pullRequestMergeTarget(issue connector.Issue) (connector.PullRequestMergeTarget, bool) {
	pr := issue.PullRequest
	if pr == nil {
		return connector.PullRequestMergeTarget{}, false
	}
	repository := pullRequestRepository(issue)
	target := connector.PullRequestMergeTarget{
		Repository: repository, Number: pullRequestNumber(issue), HeadSHA: strings.TrimSpace(pr.HeadSHA),
		BaseRepository: firstNonBlank(pr.BaseRepository, repository), BaseRef: strings.TrimSpace(pr.BaseRef),
	}
	return target, target.Repository != "" && target.Number > 0 && target.HeadSHA != "" && target.BaseRef != ""
}

func sameMergeRevision(checked, current connector.Issue) bool {
	want, valid := pullRequestMergeTarget(checked)
	got, ok := pullRequestMergeTarget(current)
	return valid && ok && strings.EqualFold(want.Repository, got.Repository) && want.Number == got.Number &&
		want.HeadSHA == got.HeadSHA && strings.EqualFold(want.BaseRepository, got.BaseRepository) && want.BaseRef == got.BaseRef
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
