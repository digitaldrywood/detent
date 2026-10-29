package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
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
	ctx, cancel := context.WithTimeout(ctx, progresscheck.MaxDuration)
	defer cancel()
	request, owner, err := source.RetryProgress(ctx, run.Issue)
	if err != nil {
		return err
	}
	backend := &retryProgressBackend{runner: o.progressRunner, run: *run}
	defer func() {
		if returnErr != nil && backend.called && run.Execution != nil {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			returnErr = errors.Join(returnErr, run.Execution.Finish(finishCtx, "failed"))
		}
	}()
	record, checked, err := progresscheck.Check(ctx, request, backend, owner)
	if err != nil {
		return err
	}
	if !checked {
		return nil
	}
	run.ProgressDecision = record
	run.PriorAttempt.Source = "Progress check comment " + record.CommentID
	run.PriorAttempt.Reason = strings.TrimSpace(run.PriorAttempt.Reason + "\n" + record.Decision.Reason)
	previousMode := run.Mode
	switch record.Decision.Outcome {
	case progresscheck.Progressing:
	case progresscheck.Reroute:
		switch record.Decision.Step {
		case RunModeImplement, RunModeMerge:
			run.Mode = record.Decision.Step
		default:
			return errors.New("progress decision cannot be dispatched")
		}
	case progresscheck.SystemDefect:
		if !backend.called {
			return nil
		}
		return fmt.Errorf("progress check filed dependency %s", record.DefectIssueID)
	default:
		return errors.New("progress decision cannot be dispatched")
	}
	if backend.called || run.Mode != previousMode {
		if admission, ok := o.scheduling.(interface {
			ReadmitRetryProgress(context.Context, RunRequest, runpkg.ProviderCapacityResolver) (runpkg.Execution, error)
		}); ok {
			execution, err := admission.ReadmitRetryProgress(ctx, *run, o.providerCapacity)
			if err != nil {
				return fmt.Errorf("admit progress step: %w", err)
			}
			run.Execution = execution
		}
	}
	return nil
}
