package nativechange

import (
	"context"
	"errors"

	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ClaimState(ctx context.Context) (executioncontract.ExecutionState, error) {
	var observation *tracker.NativeRuntimeObservation
	localWorker := e.hasWorkers()
	e.cancelMu.Lock()
	controller := e.cancel != nil
	e.cancelMu.Unlock()
	e.diagnosticMu.Lock()
	pending := e.diagnostic.OperationPending
	e.diagnosticMu.Unlock()
	if pending || !e.mu.TryLock() {
		return executioncontract.ExecutionState{Override: 1}, nil
	}
	pending = e.pending != nil || e.data.Outcome != ""
	landed := e.landed
	if e.data.Runtime != nil {
		runtime := *e.data.Runtime
		observation = &runtime
	}
	// The controller owns bounded finalization after the turn ends. Its
	// turn-end heartbeat cannot expire a live publication or review.
	outage := !e.completionOutageDeadline.IsZero()
	if observation != nil && observation.Phase == "completed" && (controller || localWorker) && !outage {
		e.mu.Unlock()
		return executioncontract.ExecutionState{Override: 1}, nil
	}
	if outage && e.scheduler.now().Before(e.completionOutageDeadline) && e.preparedOutcome != "" {
		e.mu.Unlock()
		return executioncontract.ExecutionState{Override: 1}, nil
	}
	if outage && !e.scheduler.now().Before(e.completionOutageDeadline) && pending {
		e.mu.Unlock()
		return executioncontract.ExecutionState{Override: -1}, nil
	}
	if !pending && observation != nil && observation.Phase == "completed" &&
		(observation.HeartbeatAt.IsZero() || !e.scheduler.now().Before(observation.HeartbeatAt.Add(e.scheduler.leaseTTL))) {
		e.RetainCompletion()
		e.preparedOutcome = "failed"
		e.preparedDisposition = nil
		e.preparedFailure = e.finalizationFailure(errors.New("completed worker runtime expired before finalization"))
		if outage {
			e.preparedFailure.ErrorClass = "publication"
			e.preparedFailure.Error = "Hub unavailable during completion finalization"
			e.preparedFailure.Decision = &tracker.FailureAttribution{Outcome: "instance", Reason: "Hub transport outage observed during completion finalization"}
			failure := e.preparedFailure.Public()
			e.preparedFailure = &failure
		}
		err := e.finish(ctx, "failed")
		finished := e.data.Outcome != ""
		e.mu.Unlock()
		if finished {
			e.refreshReasons(ctx, true)
		}
		return executioncontract.ExecutionState{Override: -1}, err
	}
	prepared := e.preparedOutcome != ""
	e.mu.Unlock()
	if pending || prepared {
		return executioncontract.ExecutionState{Override: 1}, nil
	}

	return executioncontract.ExecutionState{LocalWorker: localWorker, Landed: landed, Observation: observation}, nil
}
func (e *nativeExecution) CompletionData() tracker.NativeRunData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.data
}
func (e *nativeExecution) FinishPrepared(ctx context.Context, reason string) error {
	return e.finishPrepared(ctx, reason)
}
func (e *nativeExecution) Cancel(cause error) { e.cancelExecution(cause) }
func (e *nativeExecution) OwnsWork() bool {
	pending := e.hasWorkers() || e.completionOwner.Load()
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	return pending || e.cancel != nil
}
func (e *nativeExecution) LandedStatus() (bool, bool) {
	if !e.mu.TryLock() {
		return false, false
	}
	defer e.mu.Unlock()
	return e.landed, true
}
