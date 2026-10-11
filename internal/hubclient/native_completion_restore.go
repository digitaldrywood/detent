package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeCompletionState struct {
	Publication              *tracker.NativePRPublication   `json:"publication,omitempty"`
	Lease                    tracker.NativeLease            `json:"lease"`
	Data                     tracker.NativeRunData          `json:"data"`
	Pending                  *tracker.NativeRunEvent        `json:"pending,omitempty"`
	Diff                     *tracker.AttemptDiffRequest    `json:"diff,omitempty"`
	StoredSeq                int64                          `json:"stored_seq"`
	WorktreeState            string                         `json:"worktree_state"`
	WorktreeHead             string                         `json:"worktree_head"`
	Repository               string                         `json:"repository"`
	SourceRequired           bool                           `json:"source_required,omitempty"`
	RetainedSource           *tracker.ChangeSourceCapture   `json:"retained_source,omitempty"`
	RecoveredSource          *tracker.NativeChangeReference `json:"recovered_source,omitempty"`
	ConversationContinuation bool                           `json:"conversation_continuation"`
	PreparedOutcome          string                         `json:"prepared_outcome"`
	PreparedMessage          string                         `json:"prepared_message"`
	PreparedDisposition      *tracker.NativeDisposition     `json:"prepared_disposition,omitempty"`
}

func (e *nativeExecution) CompletionState() json.RawMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	data, err := json.Marshal(nativeCompletionState{
		Publication: e.publication,
		Lease:       e.claim.lease, Data: e.data, Pending: e.pending,
		Diff: e.lastDiff, StoredSeq: e.storedSeq,
		WorktreeState: e.worktreeState, WorktreeHead: e.worktreeHead,
		Repository: e.repository, ConversationContinuation: e.conversationContinuation,
		SourceRequired: e.sourceRequired, RetainedSource: e.retainedSource,
		RecoveredSource: e.recoveredSource,
		PreparedOutcome: e.preparedOutcome, PreparedMessage: e.preparedMessage,
		PreparedDisposition: e.preparedDisposition,
	})
	if err != nil {
		return nil
	}
	return data
}

func (s *Scheduler) RestoreCompletion(ctx context.Context, request orchestrator.SchedulingRequest, issue connector.Issue, saved json.RawMessage) (orchestrator.Claimed, error) {
	if !strings.HasPrefix(issue.ID, "wi_") {
		return orchestrator.Claimed{}, nil
	}
	source := s.nativeProject(request.ProjectID)
	if source == nil {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	state := nativeCompletionState{}
	if len(saved) != 0 {
		if err := json.Unmarshal(saved, &state); err != nil {
			return orchestrator.Claimed{}, errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
		}
	} else {
		fence, err := strconv.ParseInt(issue.Metadata["hub_fencing_token"], 10, 64)
		if err != nil {
			return orchestrator.Claimed{}, errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
		}
		state.Lease = tracker.NativeLease{ID: tracker.LeaseID(issue.Metadata["hub_lease_id"]), FencingToken: tracker.FencingToken(fence), WorkItemID: tracker.NativeWorkItemID(issue.ID), PolicyID: issue.Metadata["hub_policy_id"], SessionID: issue.Metadata["hub_session_id"], MachineID: s.machine.ID}
		state.Data = tracker.NativeRunData{RunID: executionID("run", issue.ID), AttemptID: executionID("attempt", string(state.Lease.ID)), LeaseID: state.Lease.ID, FencingToken: state.Lease.FencingToken, PolicyID: state.Lease.PolicyID}
	}
	lease := state.Lease
	if lease.ID == "" || lease.FencingToken <= 0 || string(lease.WorkItemID) != issue.ID || lease.MachineID != s.machine.ID || lease.PolicyID != request.Policy.ID {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	s.mu.Lock()
	claim, exists := s.nativeClaims[issue.ID]
	s.mu.Unlock()
	if exists {
		if !sameCompletionLease(claim.lease, lease) || claim.source != source || claim.execution == nil {
			return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
		}
		claim.execution.mu.Lock()
		data := claim.execution.data
		claim.execution.mu.Unlock()
		if len(saved) == 0 {
			state.Data.Identity = data.Identity
		}
		if !sameCompletionRun(data, state.Data) {
			return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
		}
		if err := claim.execution.Validate(ctx); err != nil {
			return orchestrator.Claimed{}, err
		}
		return claimedIssue(issue, nativeTrackerLease(claim.lease)), nil
	}
	if err := s.CheckProjectPolicy(ctx, request.ProjectID, request.Repository, request.Policy); err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	started := s.now()
	renewed, err := source.client.Renew(ctx, lease, int64(s.leaseTTL.Seconds()))
	if err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	if !sameCompletionLease(renewed, lease) {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	owner, err := source.client.ValidateLease(ctx, renewed)
	if err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	if owner.MachineID != s.machine.ID {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	recovery, err := source.client.Recovery(ctx, lease.WorkItemID)
	if err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	var attempt *tracker.NativeAttempt
	for i := range recovery.Attempts {
		candidate := &recovery.Attempts[i]
		if candidate.AttemptID == executionID("attempt", string(lease.ID)) && candidate.LeaseID == lease.ID && candidate.FencingToken == lease.FencingToken && candidate.PolicyID == lease.PolicyID && candidate.MachineID == lease.MachineID && candidate.SessionID == lease.SessionID {
			attempt = candidate
			break
		}
	}
	if attempt == nil || attempt.Identity == nil || attempt.Checkpoint == nil {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	if len(saved) == 0 {
		state.Data = attempt.NativeRunData
		state.WorktreeState, state.WorktreeHead = attempt.Checkpoint.WorktreeState, attempt.Checkpoint.HeadSHA
		state.Repository = request.Repository
		if !strings.Contains(state.Repository, "://") && state.Repository != "" {
			state.Repository = "https://github.com/" + state.Repository
		}
		var diff tracker.AttemptDiff
		path, err := nativeAttemptDiffPath(attempt.AttemptID)
		if err != nil {
			return orchestrator.Claimed{}, err
		}
		if err := source.client.client.request(ctx, http.MethodGet, source.client.base()+path, nil, &diff); err != nil {
			return orchestrator.Claimed{}, completionRestoreError(err)
		}
		if diff.Producer.LeaseID != lease.ID || diff.Producer.FencingToken != lease.FencingToken || diff.AttemptID != attempt.AttemptID || diff.Generation.Seq < attempt.Sequence || diff.HeadSHA != state.WorktreeHead {
			return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
		}
		state.Diff = &tracker.AttemptDiffRequest{Producer: diff.Producer, Generation: diff.Generation, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Files: diff.Files}
		state.StoredSeq = diff.Generation.Seq
	} else if !sameCompletionRun(state.Data, attempt.NativeRunData) || state.Data.LeaseID != lease.ID || state.Data.FencingToken != lease.FencingToken || state.Data.PolicyID != lease.PolicyID {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	claim = nativeClaim{source: source, lease: renewed, recovery: recovery, deadline: nativeLeaseDeadline(started, renewed)}
	execution := &nativeExecution{
		publication: state.Publication,
		scheduler:   s, claim: claim, data: state.Data, pending: state.Pending,
		lastDiff: state.Diff, storedSeq: state.StoredSeq,
		worktreeState: state.WorktreeState, worktreeHead: state.WorktreeHead,
		sourceRequired: state.SourceRequired, retainedSource: state.RetainedSource,
		recoveredSource: state.RecoveredSource,
		repository:      state.Repository, conversationContinuation: state.ConversationContinuation,
		role: state.Data.Identity.Role, preparedOutcome: state.PreparedOutcome,
		preparedMessage: state.PreparedMessage, preparedDisposition: state.PreparedDisposition,
	}
	claim.execution = execution
	s.mu.Lock()
	if _, exists := s.nativeClaims[issue.ID]; exists {
		s.mu.Unlock()
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	s.nativeClaims[issue.ID] = claim
	s.claims[issue.ID] = nativeTrackerLease(renewed)
	s.claimPolicies[issue.ID] = claimPolicy{project: request.ProjectID, repository: request.Repository, descriptor: request.Policy}
	s.mu.Unlock()
	s.client.nativeLeases.Store(source.client.base()+"/"+issue.ID, renewed)
	s.syncLeaseHold(ctx)
	return claimedIssue(issue, nativeTrackerLease(renewed)), nil
}

func sameCompletionLease(current, original tracker.NativeLease) bool {
	return current.ID == original.ID && current.WorkItemID == original.WorkItemID && current.FencingToken == original.FencingToken && current.PolicyID == original.PolicyID && current.MachineID == original.MachineID && current.SessionID == original.SessionID
}

func sameCompletionRun(current, original tracker.NativeRunData) bool {
	return current.RunID == original.RunID && current.AttemptID == original.AttemptID && current.LeaseID == original.LeaseID && current.FencingToken == original.FencingToken && current.PolicyID == original.PolicyID && current.Identity != nil && original.Identity != nil && *current.Identity == *original.Identity
}

func completionRestoreError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "policy_mismatch" {
		return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	if nativeTransportUnavailable(err) || !nativeAuthorityLost(err) {
		return err
	}
	return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
}
