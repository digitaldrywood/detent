package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner/sshexec"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SSHRunRequest struct {
	SourceIteration bool
	StepReceipts    bool
	IsolationPolicy *isolation.Policy
	ExecutionTools  []AgentTool
	Request         RunRequest
	Callbacks       []string
	Recovery        *tracker.NativeRecovery
	Native          bool
	AttemptWorker   bool
	Capacity        *providercapacity.Reservation
	Sources         *SSHExecutionSources `json:"-"`
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
	wire := SSHRunRequest{IsolationPolicy: request.Issue.IsolationPolicy, Request: request}
	callbacks := map[string]bool{
		"usage": true, "activity": request.OnActivityUpdate != nil,
		"override": request.OnOverrideRejected != nil, "progress": request.ProgressProbe != nil,
		"checkpoint": request.CheckpointValidate != nil, "tools": request.AgentToolHandler != nil,
		"model_permit": request.AcquireModelPermit != nil, "artifact_spool": spoolsArtifactLog(request.Execution),
	}
	for name, enabled := range callbacks {
		if enabled {
			wire.Callbacks = append(wire.Callbacks, name)
		}
	}
	if request.Execution != nil {
		_, wire.SourceIteration = request.Execution.(NativeSourceIterationExecution)
		_, wire.StepReceipts = stepExecution(request.Execution)
		recovery := request.Execution.Recovery()
		wire.Recovery = &recovery
		_, wire.Native = request.Execution.(ArtifactSourceExecution)
		_, wire.AttemptWorker = request.Execution.(AttemptWorkerExecution)
		if source, ok := request.Execution.(ToolExecution); ok {
			wire.ExecutionTools, _ = source.AgentTools()
		}
		if capacity, ok := request.Execution.(ProviderCapacityExecution); ok {
			wire.Capacity = capacity.ProviderCapacity()
		}
	}
	return wire
}

func (wire SSHRunRequest) Bind(ctx context.Context, peer *SSHPeer) RunRequest {
	request := wire.Request
	request.observationPeer = peer
	request.Issue.IsolationPolicy = wire.IsolationPolicy
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
		execution := &sshExecution{peer: peer, recovery: *wire.Recovery, stepReceipts: wire.StepReceipts}
		request.Execution = execution
		if wire.Native {
			sources := wire.Sources
			if sources == nil {
				sources = &SSHExecutionSources{}
			}
			native := &sshNativeExecution{
				spoolArtifacts: slices.Contains(wire.Callbacks, "artifact_spool"),
				sshExecution:   execution, sources: sources, capacity: wire.Capacity, tools: wire.ExecutionTools,
			}
			request.Execution = native
			if wire.SourceIteration {
				request.Execution = &sshSourceIterationExecution{sshNativeExecution: native}
			}
		}
	}
	return request
}

// NewSSHCallbackHandler keeps database writes and admission decisions on the
// orchestrator. Remote PIDs are deliberately absent from central usage updates.
func NewSSHCallbackHandler(request RunRequest, sessions SessionStore, checker BudgetChecker, estimator DispatchEstimator) func(context.Context, string, []json.RawMessage) (any, error) {
	return func(ctx context.Context, method string, arguments []json.RawMessage) (any, error) {
		ctx = bindSessionExecution(ctx, sessions, request.Execution)
		if strings.HasPrefix(method, "store.") {
			name := strings.TrimPrefix(method, "store.")
			switch name {
			case "StartSession":
				return invokeSSHMethod(ctx, func(ctx context.Context, start store.SessionStart) (int64, error) {
					if start.Worker != nil || sessions == nil {
						return 0, errors.New("generic SSH session start cannot register a worker")
					}
					return sessions.StartSession(ctx, start)
				}, "", arguments)
			case "FinishSession":
				return invokeSSHMethod(ctx, func(ctx context.Context, id int64, finish store.SessionFinish) error {
					if finish.Reap != nil || sessions == nil {
						return errors.New("generic SSH session finish cannot reap a worker")
					}
					return sessions.FinishSession(ctx, id, finish)
				}, "", arguments)
			case "ReconcileSessionFinalization":
				return invokeSSHMethod(ctx, func(ctx context.Context, id int64, value *sessionmodel.Finalization) error {
					owner, ok := sessions.(sessionFinalizationStore)
					if !ok || value != nil && value.Finish.Reap != nil {
						return errors.New("generic SSH session finalization cannot reap a worker")
					}
					return owner.ReconcileSessionFinalization(ctx, id, value)
				}, "", arguments)
			case "RecordUsageEvent", "UpdateSessionIdentity", "UpdateSessionProviderIdentity", "UpdateSessionResumeState", "RecordWorkflowPhaseEvent", "SaveWorkflowActivityProfile", "SessionProgress", "SaveSessionProgress", "SessionPolicy", "LatestAgentResumeState", "LatestIssueAgentResumeState", "ListSecurityAuditDispositions":
				return invokeSSHMethod(ctx, sessions, name, arguments)
			default:
				return nil, errors.New("unsupported SSH store method")
			}
		}
		if strings.HasPrefix(method, "execution.") {
			name := strings.TrimPrefix(method, "execution.")
			switch name {
			case "RecordStep", "Step":
				if _, ok := request.Execution.(StepExecution); !ok {
					return nil, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "PrepareFinish":
				if _, ok := request.Execution.(CompletionExecution); !ok {
					return nil, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "RecordPipelineTiming":
				if _, ok := request.Execution.(PipelineExecution); !ok {
					return nil, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "tools":
				source, ok := request.Execution.(ToolExecution)
				if !ok {
					return nil, errors.New("execution tools are unavailable")
				}
				_, handler := source.AgentTools()
				return invokeSSHMethod(ctx, handler, "", arguments)
			case "ValidatorVersion":
				if _, ok := request.Execution.(NativeValidatorExecution); !ok {
					return NativeValidation{}, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "SecurityAuditVersion":
				if _, ok := request.Execution.(NativeSecurityAuditExecution); !ok {
					return NativeValidation{}, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "ReviewVersion":
				if _, ok := request.Execution.(NativeReviewExecution); !ok {
					return NativeValidation{}, nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "PrepareNoChange":
				execution, ok := request.Execution.(NoChangeExecution)
				if !ok {
					return struct {
						Message  string
						Eligible bool
					}{}, nil
				}
				if len(arguments) != 1 {
					return nil, errors.New("invalid no-change source arguments")
				}
				var base string
				if err := json.Unmarshal(arguments[0], &base); err != nil {
					return nil, err
				}
				message, eligible, err := execution.PrepareNoChange(ctx, base)
				return struct {
					Message  string
					Eligible bool
				}{message, eligible}, err
			case "PhaseReceipts", "VerifyPhaseAuthority", "RecordPhaseReceipt", "StartPhased", "SetPhaseProvider", "LatestRoundVerdict", "RecordRoundVerdict", "FreezeExecutionChecklist", "ExecutionChecklist", "RecordLoopIntervention":
				if _, ok := request.Execution.(PhaseReceiptExecution); !ok && name == "PhaseReceipts" {
					return (*tracker.NativePhaseContinuation)(nil), nil
				}
				return invokeSSHMethod(ctx, request.Execution, name, arguments)
			case "SourceReview", "PrepareSourceIteration", "PrepareReview", "RecordSourceValidation", "Validate", "Start", "Checkpoint", "RecordValidator", "RecordReview", "RecordSecurityAudit", "PrepareArtifacts", "ArtifactLog", "SpoolArtifactLog", "FinalizeArtifacts", "FinalizeArtifactsWithSpoolCount", "PublishValidationEvidence", "SetRepository", "LandingTarget", "RecordLanding", "RecordUsage", "AvailabilityDeadline", "ObserveRuntime", "StartLanding", "ObserveLanding", "RecoverChangeSource", "ReconcileReleaseDelivery":
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
	peer         *SSHPeer
	recovery     tracker.NativeRecovery
	stepReceipts bool
}

func (e *sshExecution) StepReceiptsEnabled() bool { return e.stepReceipts }

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

func (e *sshExecution) RecordStep(ctx context.Context, receipt tracker.NativeStepReceipt) error {
	return e.peer.Call(ctx, "execution.RecordStep", nil, receipt)
}

func (e *sshExecution) Step(ctx context.Context, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	var receipt tracker.NativeStepReceipt
	err := e.peer.Call(ctx, "execution.Step", &receipt, binding)
	return receipt, err
}

// The central owner finishes execution even if the SSH host disappears.
func (*sshExecution) Finish(context.Context, string) error { return nil }
func (e *sshExecution) Recovery() tracker.NativeRecovery   { return e.recovery }

type SSHBudgetGuard = sshexec.BudgetGuard

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

func (r *Runner) WorkerWorkflowSnapshot() (config.Workflow, bool, bool) {
	workflow, _, checker, estimator := r.runtimeSnapshot()
	return workflow, checker != nil, estimator != nil
}

func (r *Runner) SSHWorkflow(ctx context.Context) (config.Workflow, bool, bool, error) {
	workflow, checker, estimator := r.WorkerWorkflowSnapshot()
	resolved, err := ResolveSSHWorkflow(ctx, workflow)
	return resolved, checker, estimator, err
}

func (r *Runner) SSHCallbackHandler(request RunRequest) func(context.Context, string, []json.RawMessage) (any, error) {
	_, _, checker, estimator := r.runtimeSnapshot()
	forward := request.OnUsageUpdate
	request.OnUsageUpdate = func(update UsageUpdate) error {
		r.providerUsage.Observe(update.RuntimeIdentity.BackendID, update.RateLimits)
		if forward == nil {
			return nil
		}
		return forward(update)
	}
	handler := NewSSHCallbackHandler(request, r.store, checker, estimator)
	receipts := r.goTestReceipts.Store
	return func(ctx context.Context, method string, arguments []json.RawMessage) (any, error) {
		switch method {
		case sshGoTestReceiptAttest:
			return invokeSSHMethod(ctx, receipts, "Attest", arguments)
		case sshGoTestReceiptLookup:
			return invokeSSHMethod(ctx, receipts, "Lookup", arguments)
		case sshGoTestReceiptRecord:
			return invokeSSHMethod(ctx, receipts, "Record", arguments)
		}
		return handler(ctx, method, arguments)
	}
}

const (
	sshGoTestReceiptAttest = sshexec.GoTestReceiptAttest
	sshGoTestReceiptLookup = sshexec.GoTestReceiptLookup
	sshGoTestReceiptRecord = sshexec.GoTestReceiptRecord
)

type SSHGoTestReceipts = sshexec.GoTestReceipts

func (r *Runner) GoTestReceiptsEnabled() bool {
	return r.goTestReceipts.Store != nil
}

type SSHSessionStore = sshexec.SessionStore
