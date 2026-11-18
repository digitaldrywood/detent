package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Authority interface {
	MachineID() tracker.MachineID
	Now() time.Time
	LeaseDuration() time.Duration
	ExecutionSource(string) *api.NativeConnector
	ExecutionClaim(string) (executioncontract.ExecutionClaim, bool)
	CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error
	RegisterExecution(context.Context, orchestrator.SchedulingRequest, connector.Issue, executioncontract.ExecutionClaim, executioncontract.Execution) error
}

type Factory func(context.Context, executioncontract.ExecutionClaim, State) (executioncontract.Execution, error)

func RestoreCompletion(factory Factory, host Authority, ctx context.Context, request orchestrator.SchedulingRequest, issue connector.Issue, saved json.RawMessage) (orchestrator.Claimed, error) {
	machineID := host.MachineID()

	if !strings.HasPrefix(issue.ID, "wi_") {
		return orchestrator.Claimed{}, nil
	}
	source := host.ExecutionSource(request.ProjectID)
	if source == nil {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	state := State{}
	if len(saved) != 0 {
		if err := json.Unmarshal(saved, &state); err != nil {
			return orchestrator.Claimed{}, errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
		}
		if landing := state.PreparedLanding; landing != nil {
			f := state.PreparedFinalization
			if f == nil || landing.ChangeID != f.ChangeID || landing.VersionID != f.VersionID || landing.HeadSHA != f.HeadSHA {
				return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
			}
		}
	} else {
		fence, err := strconv.ParseInt(issue.Metadata["hub_fencing_token"], 10, 64)
		if err != nil {
			return orchestrator.Claimed{}, errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
		}
		state.Lease = tracker.NativeLease{ID: tracker.LeaseID(issue.Metadata["hub_lease_id"]), FencingToken: tracker.FencingToken(fence), WorkItemID: tracker.NativeWorkItemID(issue.ID), PolicyID: issue.Metadata["hub_policy_id"], SessionID: issue.Metadata["hub_session_id"], MachineID: machineID}
		state.Data = tracker.NativeRunData{RunID: api.ExecutionID("run", issue.ID), AttemptID: api.ExecutionID("attempt", string(state.Lease.ID)), LeaseID: state.Lease.ID, FencingToken: state.Lease.FencingToken, PolicyID: state.Lease.PolicyID}
	}
	lease := state.Lease
	if lease.ID == "" || lease.FencingToken <= 0 || string(lease.WorkItemID) != issue.ID || lease.MachineID != machineID || lease.PolicyID != request.Policy.ID {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	view, exists := host.ExecutionClaim(issue.ID)
	claim := view
	if exists {
		if !SameLease(claim.Lease, lease) || claim.Source != source || claim.Execution == nil {
			return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
		}
		data := claim.Execution.CompletionData()
		if source, ok := claim.Execution.(interface{ CompletionSnapshot() State }); ok {
			data = source.CompletionSnapshot().Data
		}
		if len(saved) == 0 {
			state.Data.Identity = data.Identity
		}
		if !SameRun(data, state.Data) {
			return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
		}
		claim.Execution.RetainCompletion()
		if err := claim.Execution.Validate(ctx); err != nil {
			return orchestrator.Claimed{}, err
		}
		return api.ClaimedIssue(issue, api.NativeTrackerLease(claim.Lease)), nil
	}
	if err := host.CheckProjectPolicy(ctx, request.ProjectID, request.Repository, request.Policy); err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	started := host.Now()
	renewed, err := source.NativeClient().Renew(ctx, lease, int64(host.LeaseDuration().Seconds()))
	if err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	if !SameLease(renewed, lease) {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	owner, err := source.NativeClient().ValidateLease(ctx, renewed)
	if err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	if owner.MachineID != machineID {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	recovery, err := source.NativeClient().Recovery(ctx, lease.WorkItemID)
	if err != nil {
		return orchestrator.Claimed{}, completionRestoreError(err)
	}
	var attempt *tracker.NativeAttempt
	for i := range recovery.Attempts {
		candidate := &recovery.Attempts[i]
		if candidate.AttemptID == api.ExecutionID("attempt", string(lease.ID)) && candidate.LeaseID == lease.ID && candidate.FencingToken == lease.FencingToken && candidate.PolicyID == lease.PolicyID && candidate.MachineID == lease.MachineID && candidate.SessionID == lease.SessionID {
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
		path, err := api.NativeAttemptDiffPath(attempt.AttemptID)
		if err != nil {
			return orchestrator.Claimed{}, err
		}
		if err := source.NativeClient().Transport().Request(ctx, http.MethodGet, source.NativeClient().Scope()+path, nil, &diff); err != nil {
			return orchestrator.Claimed{}, completionRestoreError(err)
		}
		if diff.Producer.LeaseID != lease.ID || diff.Producer.FencingToken != lease.FencingToken || diff.AttemptID != attempt.AttemptID || diff.Generation.Seq < attempt.Sequence || diff.HeadSHA != state.WorktreeHead {
			return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
		}
		state.Diff = &tracker.AttemptDiffRequest{Producer: diff.Producer, Generation: diff.Generation, BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Files: diff.Files}
		state.StoredSeq = diff.Generation.Seq
	} else if !SameRun(state.Data, attempt.NativeRunData) || state.Data.LeaseID != lease.ID || state.Data.FencingToken != lease.FencingToken || state.Data.PolicyID != lease.PolicyID {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	claim = executioncontract.ExecutionClaim{Source: source, Lease: renewed, Recovery: recovery, Deadline: api.NativeLeaseDeadline(started, renewed)}
	execution, err := factory(ctx, executioncontract.ExecutionClaim{Source: source, Lease: renewed, Recovery: recovery, Deadline: claim.Deadline}, state)
	if err != nil {
		return orchestrator.Claimed{}, err
	}

	execution.RetainCompletion()
	claim.Execution = execution
	if err := host.RegisterExecution(ctx, request, issue, executioncontract.ExecutionClaim{Source: source, Lease: renewed, Recovery: recovery, Deadline: claim.Deadline}, execution); err != nil {
		return orchestrator.Claimed{}, err
	}
	return api.ClaimedIssue(issue, api.NativeTrackerLease(renewed)), nil
}

func SameLease(current, original tracker.NativeLease) bool {
	return current.ID == original.ID && current.WorkItemID == original.WorkItemID && current.FencingToken == original.FencingToken && current.PolicyID == original.PolicyID && current.MachineID == original.MachineID && current.SessionID == original.SessionID
}

func SameRun(current, original tracker.NativeRunData) bool {
	return current.RunID == original.RunID && current.AttemptID == original.AttemptID && current.LeaseID == original.LeaseID && current.FencingToken == original.FencingToken && current.PolicyID == original.PolicyID && current.Identity != nil && original.Identity != nil && *current.Identity == *original.Identity
}

func completionRestoreError(err error) error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "policy_mismatch" {
		return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	if api.NativeTransportUnavailable(err) || !api.NativeAuthorityLost(err) {
		return err
	}
	return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
}
