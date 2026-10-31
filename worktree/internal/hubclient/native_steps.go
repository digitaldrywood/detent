package hubclient

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) RecordStep(ctx context.Context, receipt tracker.NativeStepReceipt) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recordStep(ctx, receipt)
}

func (e *nativeExecution) recordStep(ctx context.Context, receipt tracker.NativeStepReceipt) error {
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
	return e.append(ctx, "run.checkpointed", "", checkpoint)
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
				return restored, e.recordStep(ctx, receipt)
			}
		}
	}
	return tracker.NativeStepReceipt{}, nil
}

func (e *nativeExecution) stepReviewState() string {
	recovery := e.claim.recovery
	if e.stepReviews != nil {
		recovery.ChangeDetail = e.stepReviews
	}
	if e.stepDiscussion != nil {
		recovery.Discussion = e.stepDiscussion
	}
	return tracker.NativeStepReviewState(recovery)
}

func (e *nativeExecution) stepReviewEpoch() tracker.FencingToken {
	epoch := tracker.NativeStepReviewEpoch(e.claim.recovery)
	for _, receipt := range e.data.Steps {
		epoch = max(epoch, receipt.ReviewEpoch)
	}
	return epoch
}
