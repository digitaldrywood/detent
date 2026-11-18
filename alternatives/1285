package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ExecutionState struct {
	Override    int
	LocalWorker bool
	Landed      bool
	Observation *tracker.NativeRuntimeObservation
}
type Execution interface {
	runner.Execution
	DiagnosticAttempt(runnerauth.DiagnosticAttempt) runnerauth.DiagnosticAttempt
	RetainCompletion()
	FinishPrepared(context.Context, string) error
	CompletionData() tracker.NativeRunData
	ClaimState(context.Context) (ExecutionState, error)
	OwnsWork() bool
	Cancel(error)
	LandedStatus() (bool, bool)
	ObserveRuntime(context.Context, tracker.NativeRuntimeObservation) error
}
type ExecutionClaim struct {
	Lost            bool
	Source          *api.NativeConnector
	Lease           tracker.NativeLease
	Recovery        tracker.NativeRecovery
	LandingCapacity int
	Deadline        time.Time
	Execution       Execution
}
type ExecutionHost interface {
	Now() time.Time
	LeaseDuration() time.Duration
	Transport() *api.Client
	MachineID() tracker.MachineID
	ExecutionSource(string) *api.NativeConnector
	ExecutionClaim(string) (ExecutionClaim, bool)
	RegisterExecution(context.Context, orchestrator.SchedulingRequest, connector.Issue, ExecutionClaim, Execution) error
	CheckProjectPolicy(context.Context, string, string, policy.Descriptor) error
	ValidateExecutionPolicy(context.Context, string, string) error
	PinnedExecutionPolicy(string) policy.Descriptor
	ExecutionError(context.Context, string, tracker.FencingToken, error) error
	CleanupExecution(context.Context, string, tracker.FencingToken)
	RenewExecution(context.Context, tracker.NativeLease, int) error
	RemainingExecution(tracker.NativeLease) time.Duration
	ExecutionAvailability(tracker.NativeLease) time.Time
	SetExecutionAvailability(tracker.NativeLease, time.Time)
	ProviderObservations() func() ([]providercapacity.Report, error)
}
type ExecutionFactory func(ExecutionHost, ExecutionClaim) Execution
type CompletionRestorer func(ExecutionHost, context.Context, orchestrator.SchedulingRequest, connector.Issue, json.RawMessage) (orchestrator.Claimed, error)
