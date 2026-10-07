package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type SSHRunRequest struct {
	BatchedLanding bool
	Request        RunRequest
	Callbacks      []string
	Recovery       *tracker.NativeRecovery
	Native         bool
	Capacity       *providercapacity.Reservation
	Sources        *SSHExecutionSources `json:"-"`
}

type SSHRunResponse struct {
	Result  RunResult
	Failure *sshError
}

func NewSSHRunResponse(result RunResult, err error) SSHRunResponse {
	return SSHRunResponse{Result: result, Failure: encodeSSHError(err)}
}
func (r SSHRunResponse) Err() error { return r.Failure.err() }

func (r *Runner) SSHHosts() []string {
	workflow, _, _, _ := r.runtimeSnapshot()
	return append([]string(nil), workflow.Config.Worker.SSHHosts...)
}

func NewSSHRunRequest(request RunRequest) SSHRunRequest {
	wire := SSHRunRequest{Request: request, BatchedLanding: request.LandingBatch != nil}
	callbacks := map[string]bool{
		"usage": request.OnUsageUpdate != nil, "activity": request.OnActivityUpdate != nil,
		"override": request.OnOverrideRejected != nil, "progress": request.ProgressProbe != nil,
		"checkpoint": request.CheckpointValidate != nil, "tools": request.AgentToolHandler != nil,
		"model_permit": request.AcquireModelPermit != nil,
	}
	for name, enabled := range callbacks {
		if enabled {
			wire.Callbacks = append(wire.Callbacks, name)
		}
	}
	if request.Execution != nil {
		recovery := request.Execution.Recovery()
		wire.Recovery = &recovery
		_, wire.Native = request.Execution.(ArtifactSourceExecution)
		if capacity, ok := request.Execution.(ProviderCapacityExecution); ok {
			wire.Capacity = capacity.ProviderCapacity()
		}
	}
	return wire
}

func (wire SSHRunRequest) Bind(ctx context.Context, peer *SSHPeer) RunRequest {
	request := wire.Request
	for _, name := range wire.Callbacks {
		switch name {
		case "usage":
			request.OnUsageUpdate = func(update UsageUpdate) error { return peer.Call(ctx, "usage", nil, update) }
		case "activity":
			request.OnActivityUpdate = func(update AgentActivityUpdate) error { return peer.Call(ctx, "activity", nil, update) }
		case "override":
			request.OnOverrideRejected = func(update []AgentOverrideRejection) error { return peer.Call(ctx, "override", nil, update) }
		case "progress":
			request.ProgressProbe = func(ctx context.Context) (string, error) {
				var result string
				err := peer.Call(ctx, "progress", &result)
				return result, err
			}
		case "checkpoint":
			request.CheckpointValidate = func(ctx context.Context) error { return peer.Call(ctx, "checkpoint", nil) }
		case "model_permit":
			request.AcquireModelPermit = func(ctx context.Context) error { return peer.Call(ctx, "model_permit", nil) }
		case "tools":
			request.AgentToolHandler = func(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
				var result AgentToolResult
				err := peer.Call(ctx, "tools", &result, call)
				return result, err
			}
		}
	}
	if wire.Recovery != nil {
		execution := &sshExecution{peer: peer, recovery: *wire.Recovery}
		request.Execution = execution
		if wire.Native {
			sources := wire.Sources
			if sources == nil {
				sources = &SSHExecutionSources{}
			}
			request.Execution = &sshNativeExecution{batchedLanding: wire.BatchedLanding, sshExecution: execution, sources: sources, capacity: wire.Capacity}
		}
	}
	return request
}

// NewSSHCallbackHandler keeps database writes and admission decisions on the
// orchestrator. Remote PIDs are deliberately absent from central usage updates.
func NewSSHCallbackHandler(request RunRequest, sessions SessionStore, checker BudgetChecker, estimator DispatchEstimator) func(context.Context, string, []json.RawMessage) (any, error) {
	return func(ctx context.Context, method string, arguments []json.RawMessage) (any, error) {
		if strings.HasPrefix(method, "store.") {
			name := strings.TrimPrefix(method, "store.")
			switch name {
			case "StartSession", "FinishSession", "RecordUsageEvent", "UpdateSessionIdentity", "UpdateSessionProviderIdentity", "UpdateSessionResumeState", "RecordWorkflowPhaseEvent", "SaveWorkflowActivityProfile", "SessionProgress", "SaveSessionProgress", "SessionPolicy":
				return invokeSSHMethod(ctx, sessions, name, arguments)
			default:
				return nil, errors.New("unsupported SSH store method")
			}
		}
		if strings.HasPrefix(method, "execution.") {
			name := strings.TrimPrefix(method, "execution.")
			switch name {
			case "PrepareFinish":
				if _, ok := request.Execution.(CompletionExecution); !ok {
					return nil, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "ValidatorVersion":
				if _, ok := request.Execution.(NativeValidatorExecution); !ok {
					return NativeValidation{}, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "RecordSourceValidation", "Validate", "Start", "Checkpoint", "RecordValidator", "PrepareArtifacts", "ArtifactLog", "FinalizeArtifacts", "PublishValidationEvidence", "SetRepository", "LandingTarget", "RecordLanding", "RecordUsage", "AvailabilityDeadline", "ObserveRuntime", "StartLanding", "ObserveLanding", "RecoverChangeSource":
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			}
			return nil, errors.New("unsupported SSH execution method")
		}
		var callback any
		switch method {
		case "usage":
			if len(arguments) != 1 {
				return nil, errors.New("invalid SSH usage arguments")
			}
			var update UsageUpdate
			if err := json.Unmarshal(arguments[0], &update); err != nil {
				return nil, err
			}
			update.WorkerProcess = procgroup.Identity{}
			if request.OnUsageUpdate == nil {
				return nil, nil
			}
			return nil, request.OnUsageUpdate(update)
		case "activity":
			callback = request.OnActivityUpdate
		case "override":
			callback = request.OnOverrideRejected
		case "progress":
			callback = request.ProgressProbe
		case "checkpoint":
			callback = request.CheckpointValidate
		case "tools":
			callback = request.AgentToolHandler
		case "model_permit":
			callback = request.AcquireModelPermit
		case "budget.check":
			return invokeSSHMethod(ctx, checker, "CheckDispatch", arguments)
		case "budget.estimate":
			return invokeSSHMethod(ctx, estimator, "EstimateDispatch", arguments)
		default:
			return nil, errors.New("unsupported SSH callback")
		}
		return invokeSSHMethod(ctx, callback, "", arguments)
	}
}

type sshExecution struct {
	peer     *SSHPeer
	recovery tracker.NativeRecovery
}

func (e *sshExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
func (e *sshExecution) Validate(ctx context.Context) error {
	return e.peer.Call(ctx, "execution.Validate", nil)
}
func (e *sshExecution) Start(ctx context.Context, identity tracker.NativeExecutionIdentity) error {
	return e.peer.Call(ctx, "execution.Start", nil, identity)
}
func (e *sshExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	return e.peer.Call(ctx, "execution.Checkpoint", nil, checkpoint)
}

// The central owner finishes execution even if the SSH host disappears.
func (*sshExecution) Finish(context.Context, string) error { return nil }
func (e *sshExecution) Recovery() tracker.NativeRecovery   { return e.recovery }

type SSHBudgetGuard struct{ Peer *SSHPeer }

func (b SSHBudgetGuard) CheckDispatch(ctx context.Context, request budget.DispatchRequest) (budget.Decision, error) {
	var result budget.Decision
	err := b.Peer.Call(ctx, "budget.check", &result, request)
	return result, err
}
func (b SSHBudgetGuard) EstimateDispatch(ctx context.Context, project string) (budget.TokenEstimate, error) {
	var result budget.TokenEstimate
	err := b.Peer.Call(ctx, "budget.estimate", &result, project)
	return result, err
}

// ResolveSSHWorkflow resolves the selected credentials on the orchestrator,
// before crossing SSH. Configuration and secrets are sent only over stdin.
func ResolveSSHWorkflow(ctx context.Context, workflow config.Workflow) (config.Workflow, error) {
	cfg := &workflow.Config
	token, err := resolveWorkerGitHubSecret(ctx, cfg.Worker.GitHubToken, os.Getenv, defaultWorkerGitHubToken, workerGitHubTokenResolutionOptions{Timeout: time.Duration(cfg.Worker.GitHubTokenResolutionTimeoutMS) * time.Millisecond, MaxAttempts: workerGitHubTokenResolutionDefaultAttempts, RetryBackoff: workerGitHubTokenResolutionDefaultRetryDelay})
	if err != nil {
		return config.Workflow{}, err
	}
	cfg.Worker.GitHubToken = token
	credential, err := resolveWorkerGitHubSecretReference(cfg.Tracker.APIKey, os.Getenv)
	if err != nil {
		return config.Workflow{}, err
	}
	cfg.Tracker.APIKey = credential
	cfg.Worker.SSHHosts = nil
	return workflow, nil
}

func (r *Runner) SSHWorkflow(ctx context.Context) (config.Workflow, bool, bool, error) {
	workflow, _, checker, estimator := r.runtimeSnapshot()
	resolved, err := ResolveSSHWorkflow(ctx, workflow)
	return resolved, checker != nil, estimator != nil, err
}

func (r *Runner) SSHCallbackHandler(request RunRequest) func(context.Context, string, []json.RawMessage) (any, error) {
	_, _, checker, estimator := r.runtimeSnapshot()
	return NewSSHCallbackHandler(request, r.store, checker, estimator)
}

// SSHSessionStore forwards session data, but has no local process-registration
// interface: numeric PIDs from another host are never local reap authorities.
type SSHSessionStore struct{ Peer *SSHPeer }

func (s SSHSessionStore) StartSession(ctx context.Context, input store.SessionStart) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.StartSession", &result, input)
	return result, err
}
func (s SSHSessionStore) FinishSession(ctx context.Context, id int64, input store.SessionFinish) error {
	return s.Peer.Call(ctx, "store.FinishSession", nil, id, input)
}
func (s SSHSessionStore) RecordUsageEvent(ctx context.Context, input store.UsageEvent) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.RecordUsageEvent", &result, input)
	return result, err
}

func (s SSHSessionStore) UpdateSessionIdentity(ctx context.Context, id int64, input agentidentity.Identity) error {
	return s.Peer.Call(ctx, "store.UpdateSessionIdentity", nil, id, input)
}

func (s SSHSessionStore) UpdateSessionProviderIdentity(ctx context.Context, id int64, input store.SessionProviderIdentity) error {
	return s.Peer.Call(ctx, "store.UpdateSessionProviderIdentity", nil, id, input)
}

func (s SSHSessionStore) UpdateSessionResumeState(ctx context.Context, id int64, input store.SessionResumeState) error {
	return s.Peer.Call(ctx, "store.UpdateSessionResumeState", nil, id, input)
}

func (s SSHSessionStore) RecordWorkflowPhaseEvent(ctx context.Context, input store.WorkflowPhaseEvent) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.RecordWorkflowPhaseEvent", &result, input)
	return result, err
}

func (s SSHSessionStore) SaveWorkflowActivityProfile(ctx context.Context, id int64, input store.WorkflowPhaseEvent, profile workflowmetrics.ActivityProfile) (int64, error) {
	var result int64
	err := s.Peer.Call(ctx, "store.SaveWorkflowActivityProfile", &result, id, input, profile)
	return result, err
}

func (s SSHSessionStore) SessionProgress(ctx context.Context, id int64) (store.SessionProgress, error) {
	var result store.SessionProgress
	err := s.Peer.Call(ctx, "store.SessionProgress", &result, id)
	return result, err
}

func (s SSHSessionStore) SaveSessionProgress(ctx context.Context, id int64, input store.SessionProgress) error {
	return s.Peer.Call(ctx, "store.SaveSessionProgress", nil, id, input)
}

func (s SSHSessionStore) SessionPolicy(ctx context.Context, project string, id int64) (policy.Descriptor, error) {
	var result policy.Descriptor
	err := s.Peer.Call(ctx, "store.SessionPolicy", &result, project, id)
	return result, err
}
