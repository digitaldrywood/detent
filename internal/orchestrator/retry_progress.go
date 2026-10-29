package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

func (o *Orchestrator) prepareRetryProgress(ctx context.Context, run *RunRequest) error {
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
	switch record.Decision.Outcome {
	case progresscheck.Progressing:
		return nil
	case progresscheck.Reroute:
		switch record.Decision.Step {
		case RunModeImplement, RunModeMerge:
			previousMode := run.Mode
			run.Mode = record.Decision.Step
			if run.Mode != previousMode {
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
	case progresscheck.SystemDefect:
		if !backend.called {
			return nil
		}
		return fmt.Errorf("progress check filed dependency %s", record.DefectIssueID)
	}
	return errors.New("progress decision cannot be dispatched")
}
