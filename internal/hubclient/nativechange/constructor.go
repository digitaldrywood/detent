package nativechange

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func NewExecution(host executioncontract.ExecutionHost, view executioncontract.ExecutionClaim) executioncontract.Execution {
	s := authority(host)
	claim := nativeClaim{source: view.Source, lease: view.Lease, recovery: view.Recovery, landingCapacity: view.LandingCapacity, deadline: view.Deadline}
	execution := &nativeExecution{scheduler: s, claim: claim, data: tracker.NativeRunData{
		RunID: executionID("run", string(claim.lease.WorkItemID)), AttemptID: executionID("attempt", string(claim.lease.ID)),
		PolicyID: claim.lease.PolicyID, LeaseID: claim.lease.ID, FencingToken: claim.lease.FencingToken,
	}}
	for _, attempt := range claim.recovery.Attempts {
		if attempt.Identity != nil && attempt.LeaseID == claim.lease.ID && attempt.FencingToken == claim.lease.FencingToken && (attempt.Status == "running" || attempt.Status == "interrupted") {
			execution.data = attempt.NativeRunData
			if attempt.Runtime != nil {
				execution.runtimePublishedAt = attempt.Runtime.HeartbeatAt
			}
			execution.usageStartedAt = attempt.StartedAt
			execution.role = attempt.Identity.Role
			execution.reconnecting = true
			if attempt.Checkpoint != nil {
				execution.worktreeState = attempt.Checkpoint.WorktreeState
				execution.worktreeHead = attempt.Checkpoint.HeadSHA
			}
			break
		}
	}
	return execution
}

func NewRecoveredExecution(host executioncontract.ExecutionHost, ctx context.Context, view executioncontract.ExecutionClaim, state checkpoint.State) (executioncontract.Execution, error) {
	s := authority(host)
	claim := nativeClaim{source: view.Source, lease: view.Lease, recovery: view.Recovery, deadline: view.Deadline}
	execution := &nativeExecution{
		publication: state.Publication, observations: state.Observations,
		scheduler: s, claim: claim, data: state.Data, pending: state.Pending, rejected: state.Rejected,
		lastDiff: state.Diff, storedSeq: state.StoredSeq,
		worktreeState: state.WorktreeState, worktreeHead: state.WorktreeHead,
		sourceRequired: state.SourceRequired, retainedSource: state.RetainedSource,
		recoveredSource: state.RecoveredSource, mergeResolution: state.MergeResolution,
		repository: state.Repository, conversationContinuation: state.ConversationContinuation,
		preparedOutcome: state.PreparedOutcome,
		preparedMessage: state.PreparedMessage, preparedDisposition: state.PreparedDisposition,
		preparedFailure: state.Data.TerminalFailure,
	}
	if state.PreparedFailure != nil {
		execution.preparedFailure = state.PreparedFailure
	}
	if finalization := state.PreparedFinalization; finalization != nil {
		execution.change = &runner.NativeChange{
			NoChange: finalization.NoChange, Landing: state.PreparedLanding,
			Changed: finalization.Changed, Files: finalization.Files,
			ChangeID: finalization.ChangeID, VersionID: finalization.VersionID,
			BaseSHA: finalization.BaseSHA, HeadSHA: finalization.HeadSHA,
			Reviewed: finalization.Reviewed, VersionError: finalization.VersionError,
			VersionCode: finalization.VersionCode, Error: finalization.Error,
		}
		execution.settled = finalization.Settled
		execution.finalizationSource = finalization.SourceVersion
		execution.finalizationAttempt = finalization.SourceAttemptID
	}
	if execution.change != nil && execution.change.NoChange != nil {
		for _, receipt := range execution.data.Steps {
			restored, err := execution.restoreStep(ctx, receipt)
			if err != nil {
				return nil, err
			}
			switch restored.Step {
			case "review":
				execution.change.ImplementationReview = restored.Validator
			case "validator":
				execution.change.Validator = restored.Validator
			case "security_audit":
				execution.change.SecurityAudit = restored.SecurityAudit
			}
		}
	}
	if state.Data.Identity != nil {
		execution.role = state.Data.Identity.Role
	}
	return execution, nil
}
