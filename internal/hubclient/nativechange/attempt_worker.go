package nativechange

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubclient/runtimeevidence"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (e *nativeExecution) AttemptWorkspace() workspace.AttemptWorkspace {
	client := e.claim.source.NativeClient()
	e.mu.Lock()
	defer e.mu.Unlock()
	return workspace.AttemptWorkspace{
		Organization: string(client.OrganizationID()),
		Project:      string(client.ProjectID()),
		Attempt:      e.data.AttemptID,
	}
}

func (e *nativeExecution) RegisterAttemptWorker(ctx context.Context, worker tracker.NativeAttemptWorker, observation tracker.NativeRuntimeObservation) (bool, error) {
	if !worker.Valid() {
		return false, errors.New("invalid attempt worker identity")
	}
	e.workerLaunchMu.Lock()
	defer e.workerLaunchMu.Unlock()
	if e.workerFromRecovery {
		if err := e.reconcileAttemptWorker(ctx); err != nil {
			return false, err
		}
		e.workerFromRecovery = false
	}
	if e.worker != nil {
		if !e.workerExited {
			return *e.worker == worker, nil
		}
		if *e.worker == worker {
			return false, runner.ErrExecutionAuthorityUnavailable
		}
		if err := e.retireAttemptWorker(ctx, *e.worker); err != nil {
			return false, err
		}
		e.worker, e.workerExited = nil, false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending != nil && e.pending.Worker != nil {
		if *e.pending.Worker != worker {
			return false, runner.ErrExecutionAuthorityUnavailable
		}
		if err := e.flush(ctx); err != nil {
			return false, err
		}
		e.worker = &worker
		e.reconnecting = false
		return true, nil
	}
	if err := e.flush(ctx); err != nil {
		return false, err
	}

	event := "run.observed"
	if e.data.Sequence == 0 {
		if err := e.Validate(ctx); err != nil {
			return false, err
		}
		identity := tracker.WorkerBootstrapIdentity()
		if reservation := e.ProviderCapacity(); reservation != nil {
			identity = tracker.NativeExecutionIdentity{Role: reservation.Role, Backend: reservation.Backend, Model: reservation.Model}
		}
		if err := e.validateProviderStart(identity); err != nil {
			return false, err
		}
		if observation.Phase == "implementation" && identity.Role == runner.RolePlan && e.scheduler.host.PinnedExecutionPolicy(string(e.claim.lease.WorkItemID)).Gates.PlanEnabled {
			observation.Phase = "planning"
			identity = tracker.WorkerBootstrapIdentity()
		}
		issue, err := e.claim.source.NativeClient().Issue(ctx, e.claim.lease.WorkItemID)
		if err != nil {
			return false, err
		}
		recovery := e.Recovery()
		recovery.Issue.State = issue.State
		e.recovery.Store(&recovery)
		e.data.Identity = &identity
		e.workerBootstrapping = true
		event = "run.started"
	}

	if e.reconnecting {
		observation.LocalAttemptID, observation.Generation = 0, 0
		if e.data.Runtime != nil && e.data.Runtime.Landing != nil && e.data.Runtime.Landing.Waiting {
			landing := *e.data.Runtime.Landing
			landing.FromState, landing.TargetState = "", ""
			observation.Landing = &landing
		}
		e.data.Runtime = nil
		e.data.Outcome = ""
		e.data.Finalization = nil
		e.data.TerminalFailure = nil
		e.data.Disposition = nil
		e.data.CompletionBody = ""
	}
	if observation.WorkerLaunch != nil {
		launch := *observation.WorkerLaunch
		launch.Bootstrap = e.workerBootstrapping
		observation.WorkerLaunch = &launch
	}
	observation = runtimeevidence.Merge(e.data.Runtime, observation)
	e.data.Runtime = &observation
	err := e.appendEvent(ctx, event, "", nil, &worker, true)
	if nativeTransportUnavailable(err) && e.pending != nil && e.pending.Worker != nil {
		err = e.flush(ctx)
	}
	if err != nil {
		return false, err
	}
	e.worker = &worker
	e.reconnecting = false
	return true, nil
}

func (e *nativeExecution) reconcileAttemptWorker(ctx context.Context) error {
	client := e.claim.source.NativeClient()
	lease := e.claim.lease
	attempt := e.AttemptWorkspace().Attempt
	for after := ""; ; {
		page, err := client.AttemptWorkers(ctx, after)
		if err != nil {
			return fmt.Errorf("read recovered attempt worker: %w", err)
		}
		for _, record := range page.Workers {
			if record.AttemptID != attempt {
				continue
			}
			if record.WorkItemID != lease.WorkItemID || record.LeaseID != lease.ID || record.FencingToken != lease.FencingToken ||
				record.MachineID != lease.MachineID || record.LeaseState != runnerauth.AttemptLeaseLive {
				return runner.ErrExecutionAuthorityUnavailable
			}
			if err := runner.VerifyRecoveredAttemptWorkerExited(record.Worker); err != nil {
				return err
			}
			return e.retireAttemptWorker(ctx, record.Worker)
		}
		if page.Next == "" {
			return nil
		}
		if page.Next <= after {
			return runner.ErrExecutionAuthorityUnavailable
		}
		after = page.Next
	}
}

func (e *nativeExecution) AttemptWorkerExited(worker tracker.NativeAttemptWorker) {
	e.workerLaunchMu.Lock()
	defer e.workerLaunchMu.Unlock()
	if e.worker != nil && *e.worker == worker {
		e.workerExited = true
	}
}

func (e *nativeExecution) retireAttemptWorker(ctx context.Context, worker tracker.NativeAttemptWorker) error {
	event := tracker.NativeRunEvent{
		WorkerCleanup: &worker, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{
			AttemptID: e.AttemptWorkspace().Attempt, LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken,
		},
	}
	return e.claim.source.NativeClient().AppendEvent(ctx, e.claim.lease.WorkItemID, event)
}
