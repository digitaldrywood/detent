package orchestrator

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
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
}

func (b retryProgressBackend) Decide(ctx context.Context, request progresscheck.Request) (progresscheck.Decision, error) {
	return b.runner.CheckProgress(ctx, b.run, request)
}

func (o *Orchestrator) retryProgress(ctx context.Context, issue connector.Issue, mode string, decision *progresscheck.Record) (string, bool, error) {
	source, ok := o.scheduling.(retryProgressSource)
	if !ok || o.progressRunner == nil {
		return mode, true, nil
	}
	request, owner, err := source.RetryProgress(ctx, issue)
	if err != nil {
		return "", false, err
	}
	backend := retryProgressBackend{runner: o.progressRunner, run: RunRequest{Issue: issue, Mode: mode, SelectorContext: o.selectorContext()}}
	record, checked, err := progresscheck.Check(ctx, request, backend, owner)
	if err != nil {
		return "", false, err
	}
	if !checked {
		return mode, true, nil
	}
	if decision != nil {
		*decision = record
	}
	switch record.Decision.Outcome {
	case progresscheck.Progressing:
		return mode, true, nil
	case progresscheck.Reroute:
		switch record.Decision.Step {
		case RunModeImplement, RunModeMerge:
			return record.Decision.Step, true, nil
		}
	case progresscheck.SystemDefect:
		return "", false, nil
	}
	return "", false, errors.New("progress decision cannot be dispatched")
}
