package nativechange

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubclient/evidence"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) RecordStep(ctx context.Context, receipt tracker.NativeStepReceipt) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var retention *checkpointReceipt
	if receipt.Step == "validation" && receipt.Validation != nil && receipt.Source.HeadSHA != "" && receipt.Validation.HeadSHA == receipt.Source.HeadSHA {
		retention = &checkpointReceipt{head: receipt.Source.HeadSHA, workspaceDigest: receipt.Source.WorkspaceDigest}
	}
	return e.recordStep(ctx, receipt, retention)
}

func (e *nativeExecution) recordStep(ctx context.Context, receipt tracker.NativeStepReceipt, retention *checkpointReceipt) error {
	if err := e.flush(ctx); err != nil {
		if retention == nil || e.pending == nil || e.pending.Type != "run.observed" || !errors.Is(err, runner.ErrOperatorStopped) {
			return err
		}
		retention.observation = e.pending
		e.pending, e.observations = nil, nil
	}
	receipt.PolicyID = e.data.PolicyID
	receipt.ReviewState = e.stepReviewState()
	receipt.ReviewEpoch = e.stepReviewEpoch()
	steps := slices.Clone(e.data.Steps)
	found := false
	for i := range steps {
		if steps[i].Step == receipt.Step {
			steps[i], found = receipt, true
			break
		}
	}
	if !found {
		steps = append(steps, receipt)
	}
	e.data.Steps = steps
	checkpoint := e.data.Handoff
	if receipt.Step == "agent_turn" || receipt.Step == "commit" {
		checkpoint = &receipt.Source
	}
	return e.appendEvent(ctx, "run.checkpointed", "", checkpoint, retention)
}

func (e *nativeExecution) Step(ctx context.Context, binding tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	if err := e.Validate(ctx); err != nil {
		return tracker.NativeStepReceipt{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	binding.PolicyID, binding.ReviewState = e.data.PolicyID, e.stepReviewState()
	binding.ReviewEpoch = e.stepReviewEpoch()
	for _, receipt := range e.data.Steps {
		if receipt.Matches(binding) {
			return e.restoreStep(ctx, receipt)
		}
	}
	for i := len(e.claim.recovery.Attempts) - 1; i >= 0; i-- {
		for _, receipt := range e.claim.recovery.Attempts[i].Steps {
			if receipt.Matches(binding) {
				restored, err := e.restoreStep(ctx, receipt)
				if err != nil {
					return tracker.NativeStepReceipt{}, err
				}
				return restored, e.recordStep(ctx, receipt, nil)
			}
		}
	}
	return tracker.NativeStepReceipt{}, nil
}

func (e *nativeExecution) stepReviewState() string {
	return evidence.ReviewState(e.claim.recovery, e.stepReviews, e.stepDiscussion)
}

func (e *nativeExecution) stepReviewEpoch() tracker.FencingToken {
	return evidence.ReviewEpoch(e.claim.recovery, e.data.Steps)
}
