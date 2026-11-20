package nativechange

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/evidence"
	"github.com/digitaldrywood/detent/internal/policy"
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
	return evidence.ReviewState(e.claim.recovery, e.stepReviews, e.stepDiscussion)
}

func (e *nativeExecution) stepReviewEpoch() tracker.FencingToken {
	return evidence.ReviewEpoch(e.claim.recovery, e.data.Steps)
}

func (e *nativeExecution) PhaseReceipts(ctx context.Context) (bool, error) {
	return e.claim.source.NativeClient().PhaseReceiptRoute(ctx, e.claim.lease.WorkItemID)
}

func (e *nativeExecution) VerifyPhaseAuthority(ctx context.Context) error {
	if err := api.MutationEffectError(ctx); err != nil {
		return err
	}
	if err := e.Validate(ctx); err != nil {
		return err
	}
	_, err := e.claim.source.NativeClient().ValidateLease(ctx, e.claim.lease)
	return e.executionError(ctx, err)
}

func (e *nativeExecution) RecordPhaseReceipt(ctx context.Context, phase, branch, head, tree, input string) error {
	lease := e.claim.lease
	e.mu.Lock()
	attemptID := e.data.AttemptID
	e.mu.Unlock()
	digest := policy.Digest([]byte(strings.Join([]string{phase, head, tree, lease.PolicyID, input}, "\x00")))
	mutation := tracker.Mutation{IdempotencyKey: attemptID + ":phase:" + digest, LeaseID: lease.ID, FencingToken: lease.FencingToken}
	payload := map[string]any{
		"idempotency_key": mutation.IdempotencyKey, "lease_id": mutation.LeaseID, "fencing_token": strconv.FormatInt(int64(mutation.FencingToken), 10),
		"attempt_id": attemptID, "phase": phase, "branch": branch, "head_sha": head, "tree_sha": tree,
		"policy_id": lease.PolicyID, "input_digest": digest,
	}
	client := e.claim.source.NativeClient()
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return err
	}
	err := client.PublishPhaseReceipt(ctx, lease.WorkItemID, payload)
	if nativeTransportUnavailable(err) && ctx.Err() == nil {
		if authorityErr := e.VerifyPhaseAuthority(ctx); authorityErr != nil {
			return errors.Join(err, authorityErr)
		}
		err = client.PublishPhaseReceipt(ctx, lease.WorkItemID, payload)
	}
	return e.executionError(ctx, err)
}
