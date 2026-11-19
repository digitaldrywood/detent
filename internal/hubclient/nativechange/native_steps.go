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
	var completed *completedReceipt
	if receipt.Step == "validation" && receipt.Validation != nil && receipt.Validation.HeadSHA != "" && receipt.Validation.HeadSHA == receipt.Source.HeadSHA {
		completed = &completedReceipt{validation: receipt.Validation}
	}
	return e.recordStep(ctx, receipt, completed)
}

func (e *nativeExecution) recordStep(ctx context.Context, receipt tracker.NativeStepReceipt, completed *completedReceipt) error {
	if completed != nil && e.pending != nil && e.pending.Type == "run.observed" && errors.Is(e.executionCause(ctx), runner.ErrOperatorStopped) {
		e.pending, e.observations = nil, nil
	}
	if err := e.flush(ctx); err != nil {
		return err
	}
	receipt.PolicyID = e.data.PolicyID
	receipt.ReviewState = e.stepReviewState()
	receipt.ReviewEpoch = e.stepReviewEpoch()
	var err error
	receipt, err = e.retainStep(ctx, receipt)
	if err != nil {
		return err
	}
	e.data.Steps = withStep(e.data.Steps, receipt)
	if completed != nil {
		completed.gate = &receipt
		if completed.validation.TimingStage == "finalization" {
			result := tracker.PublicGateResult(*completed.validation)
			e.data.Runtime = validationRuntime(e.data.Runtime, result, e.scheduler.now())
		}
	}
	checkpoint := e.data.Handoff
	if receipt.Step == "agent_turn" || receipt.Step == "commit" || checkpoint == nil && completed != nil {
		checkpoint = &receipt.Source
	}
	return e.appendReceipt(ctx, "run.checkpointed", "", checkpoint, completed)
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

func withStep(steps []tracker.NativeStepReceipt, receipt tracker.NativeStepReceipt) []tracker.NativeStepReceipt {
	steps = slices.Clone(steps)
	for i := range steps {
		if steps[i].Step == receipt.Step {
			steps[i] = receipt
			return steps
		}
	}
	return append(steps, receipt)
}
