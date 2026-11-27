package orchestrator

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

const mergeControlCheckedHeadOutput = "merge_control_checked_head"

func (o *Orchestrator) confirmMergeControlClaim(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	err := o.verifyMergeControlClaim(ctx, state.Claimed[event.IssueID], o.nativeCompletionExecution(event))
	switch {
	case err == nil:
		return ctx.Err() == nil
	case errors.Is(err, ErrSchedulingClaimLost), errors.Is(err, runpkg.ErrExecutionAuthorityUnavailable):
		o.rejectWorkerCompletion(ctx, state, event, running, "merge completion lost its claim", err)
		o.releaseClaim(state, event.IssueID)
	case ctx.Err() == nil:
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, err)
	}
	return false
}

func (o *Orchestrator) verifyMergeControlClaim(ctx context.Context, claim Claimed, execution runpkg.Execution) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if o.scheduling != nil {
		if execution == nil {
			return ErrSchedulingClaimLost
		}
		return execution.Validate(ctx)
	}
	if !o.cfg.Claiming.Enabled {
		return nil
	}
	if claim.Issue.ID == "" || claim.Owner == "" {
		return ErrSchedulingClaimLost
	}
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{claim.Issue.ID})
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.ID != claim.Issue.ID {
			continue
		}
		lease, ok := o.issueLease(issue)
		if !ok || o.leaseStale(lease, o.clockNow()) || !sameClaimOwner(o.claimWinner(issue), claim.Owner) {
			return ErrSchedulingClaimLost
		}
		return context.Cause(ctx)
	}
	return errors.New("issue missing from merge claim verification")
}

func (p dispatchPlanner) readyMergeControlCandidate(state *State, issue connector.Issue) bool {
	if !p.mechanicalMergeAdmission(issue) || !mergeWorkerProgrammaticMergeReady(issue, p.cfg) || issue.PullRequest.BaseBranchStrict || strings.TrimSpace(issue.PullRequest.BaseSHA) == "" {
		return false
	}
	if retry, ok := state.Retry[issue.ID]; ok && retry.MergePrecheck != nil {
		return false
	}
	return state.mergeReservations[issue.ID].RefreshHeadSHA != issue.PullRequest.HeadSHA
}

func sameMergeControlRevision(checked, current connector.Issue) bool {
	return checked.PullRequest != nil && current.PullRequest != nil &&
		pullRequestRepository(checked) == pullRequestRepository(current) &&
		pullRequestNumber(checked) == pullRequestNumber(current) &&
		checked.PullRequest.HeadSHA == current.PullRequest.HeadSHA &&
		!current.PullRequest.BaseBranchStrict
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
