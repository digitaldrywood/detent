package nativechange

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCompletionState = checkpoint.State

func (e *nativeExecution) CompletionSnapshot() checkpoint.State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completionSnapshot()
}
func (e *nativeExecution) completionSnapshot() checkpoint.State {
	var landing *runner.NativeLanding
	if e.change != nil {
		landing = e.change.Landing
	}
	return nativeCompletionState{
		Settled:     e.settled,
		Publication: e.publication, Observations: e.observations,
		Lease: e.claim.lease, Data: e.data, Pending: e.pending, Rejected: e.rejected,
		Diff: e.lastDiff, StoredSeq: e.storedSeq,
		WorktreeState: e.worktreeState, WorktreeHead: e.worktreeHead,
		Repository: e.repository, ConversationContinuation: e.conversationContinuation,
		SourceRequired: e.sourceRequired, RetainedSource: e.retainedSource,
		RecoveredSource: e.recoveredSource, MergeResolution: e.mergeResolution,
		PreparedOutcome: e.preparedOutcome, PreparedMessage: e.preparedMessage,
		PreparedDisposition:  e.preparedDisposition,
		PreparedFinalization: e.preparedFinalization(), PreparedFailure: e.preparedFailure,
		PreparedLanding: landing,
	}
}
func (e *nativeExecution) CompletionState() json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	data, err := json.Marshal(e.completionSnapshot())
	if err != nil {
		return nil
	}
	return data
}

func RestoreCompletion(host executioncontract.ExecutionHost, ctx context.Context, request orchestrator.SchedulingRequest, issue connector.Issue, saved json.RawMessage) (orchestrator.Claimed, error) {
	return checkpoint.RestoreCompletion(func(ctx context.Context, view executioncontract.ExecutionClaim, state checkpoint.State) (executioncontract.Execution, error) {
		return NewRecoveredExecution(host, ctx, view, state)
	}, host, ctx, request, issue, saved)
}

func sameCompletionLease(current, original tracker.NativeLease) bool {
	return checkpoint.SameLease(current, original)
}
