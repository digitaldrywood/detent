package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type retryProgressSource interface {
	RetryProgress(context.Context, connector.Issue) (progresscheck.Request, progresscheck.Owner, error)
}

type retryProgressRunner interface {
	CheckProgress(context.Context, RunRequest, progresscheck.Request) (progresscheck.Decision, error)
}

type retryProgressBackend struct {
	runner retryProgressRunner
	run    RunRequest
	called bool
}

func (b *retryProgressBackend) Decide(ctx context.Context, request progresscheck.Request) (progresscheck.Decision, error) {
	if b.run.AcquireModelPermit != nil {
		if err := b.run.AcquireModelPermit(ctx); err != nil {
			return progresscheck.Decision{}, err
		}
	}
	b.called = true
	return b.runner.CheckProgress(ctx, b.run, request)
}

func (o *Orchestrator) prepareRetryProgress(ctx context.Context, run *RunRequest) (returnErr error) {
	source, ok := o.scheduling.(retryProgressSource)
	if !ok || o.progressRunner == nil {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, progresscheck.MaxDuration)
	defer cancel()
	request, owner, err := source.RetryProgress(checkCtx, run.Issue)
	if err != nil {
		return o.continueRetryWithoutProgress(ctx, run, false, err)
	}
	backend := &retryProgressBackend{runner: o.progressRunner, run: *run}
	defer func() {
		if returnErr != nil && backend.called && run.Execution != nil {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, run.Execution.Finish(finishCtx, "failed"))
		}
	}()
	record, checked, err := progresscheck.Check(checkCtx, request, backend, owner)
	if err != nil {
		return o.continueRetryWithoutProgress(ctx, run, backend.called, err)
	}
	if !checked {
		return nil
	}
	previousMode := run.Mode
	switch record.Decision.Outcome {
	case progresscheck.Progressing:
	case progresscheck.Reroute:
		switch record.Decision.Step {
		case progresscheck.CompleteStep:
		case RunModeImplement, RunModeMerge:
			run.Mode = record.Decision.Step
		default:
			return o.continueRetryWithoutProgress(ctx, run, backend.called, errors.New("progress decision cannot be dispatched"))
		}
	case progresscheck.SystemDefect:
	default:
		return o.continueRetryWithoutProgress(ctx, run, backend.called, errors.New("progress decision cannot be dispatched"))
	}
	run.ProgressDecision = record
	run.PriorAttempt.Source = "Progress check comment " + record.CommentID
	last := request.Attempts[len(request.Attempts)-1]
	if last.LandingRefusal == "nothing_to_land" {
		run.PriorAttempt.Reason = strings.TrimSpace(run.PriorAttempt.Reason + "\nnothing_to_land: " + last.Error)
	}
	run.PriorAttempt.Reason = strings.TrimSpace(run.PriorAttempt.Reason + "\n" + record.Decision.Reason + "\nDiagnosis: " + record.Decision.Diagnosis)
	if record.Decision.Instruction != "" {
		run.PriorAttempt.Reason += "\nNext instruction: " + record.Decision.Instruction
	}
	if record.Decision.Outcome == progresscheck.SystemDefect {
		if !backend.called {
			return nil
		}
		return fmt.Errorf("progress check filed dependency %s", record.DefectIssueID)
	}
	if record.Decision.Outcome == progresscheck.Reroute && record.Decision.Step == progresscheck.CompleteStep {
		if run.Execution != nil {
			if err := run.Execution.Finish(ctx, "cancelled"); err != nil {
				return fmt.Errorf("finish progress execution: %w", err)
			}
		}
		return nil
	}
	if backend.called || run.Mode != previousMode {
		return o.readmitRetryProgress(ctx, run)
	}
	return nil
}

func (o *Orchestrator) continueRetryWithoutProgress(ctx context.Context, run *RunRequest, called bool, failure error) error {
	run.ProgressDecision = progresscheck.Record{}
	reason := "Progress check unavailable: " + failure.Error()
	run.PriorAttempt.Reason = strings.TrimSpace(run.PriorAttempt.Reason + "\n" + reason)
	if observed, ok := run.Execution.(runpkg.RuntimeExecution); ok {
		evidenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := observed.ObserveRuntime(evidenceCtx, tracker.NativeRuntimeObservation{LocalAttemptID: run.WorkAttemptID, Generation: run.Generation, Recovery: &tracker.NativeRecoveryDecision{Action: "retry", Reason: reason}})
		cancel()
		if err != nil && o.logger != nil {
			o.logger.Warn("record progress check failure", "issue_id", run.Issue.ID, "error", err)
		}
	}
	if called {
		return o.readmitRetryProgress(ctx, run)
	}
	return nil
}

func (o *Orchestrator) readmitRetryProgress(ctx context.Context, run *RunRequest) error {
	if admission, ok := o.scheduling.(interface {
		ReadmitRetryProgress(context.Context, RunRequest, runpkg.ProviderCapacityResolver) (runpkg.Execution, *isolation.Policy, error)
	}); ok {
		execution, policy, err := admission.ReadmitRetryProgress(ctx, *run, o.providerCapacity)
		if err != nil {
			return fmt.Errorf("admit progress step: %w", err)
		}
		run.Execution = execution
		run.Issue.IsolationPolicy = policy
	}
	return nil
}
