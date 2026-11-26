package nativechange

import (
	"context"
	"errors"

	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ClaimState(ctx context.Context) (executioncontract.ExecutionState, error) {
	var observation *tracker.NativeRuntimeObservation
	localWorker := e.hasWorkers()
	e.cancelMu.Lock()
	if e.unretiredProvider {
		e.cancelMu.Unlock()
		return executioncontract.ExecutionState{}, runner.ErrProviderSessionUnretired
	}
	controller := e.supervisor != nil
	lost := e.supervisionLost || controller && supervisorLost(e.supervisor.cause())
	select {
	case <-e.reviewDone:
		e.reviewDone = nil
	default:
	}
	reviewing := e.reviewDone != nil
	e.cancelMu.Unlock()
	if lost {
		return executioncontract.ExecutionState{Override: -1}, nil
	}
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
	liveReview := reviewing && e.settled && e.change != nil && e.change.VersionID != "" && e.data.Outcome == ""
	if (controller || liveReview || localWorker && observation != nil && observation.Phase == "completed") && !outage {
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
	e.cancelMu.Lock()
	unretired := e.unretiredProvider
	e.cancelMu.Unlock()
	if unretired {
		return runner.ErrProviderSessionUnretired
	}
	return e.finishPrepared(ctx, reason)
}
func (e *nativeExecution) Cancel(cause error) {
	if runner.UnretiredProviderSession(cause) {
		e.cancelMu.Lock()
		defer e.cancelMu.Unlock()
		e.unretiredProvider = true
		if e.cancel != nil {
			e.cancel(cause)
		}
		return
	}
	e.cancelExecution(cause)
}
func (e *nativeExecution) OwnsWork() bool {
	pending := e.hasWorkers() || e.completionOwner.Load() || e.preparedPublisher.Load()
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	return pending || e.cancel != nil || e.unretiredProvider
}
func (e *nativeExecution) LandedStatus() (bool, bool) {
	if !e.mu.TryLock() {
		return false, false
	}
	defer e.mu.Unlock()
	return e.landed, true
}
