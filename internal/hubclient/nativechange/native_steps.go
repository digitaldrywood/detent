package nativechange

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/evidence"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
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

func (e *nativeExecution) PhaseReceipts(ctx context.Context, command string) (*tracker.NativePhaseContinuation, error) {
	client, lease := e.claim.source.NativeClient(), e.claim.lease
	receipts, err := client.ItemPhaseReceipts(ctx, lease.WorkItemID)
	var refusal *APIError
	if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound && refusal.Code == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	attempt, reviewState := e.data.AttemptID, e.stepReviewState()
	e.mu.Unlock()
	if len(receipts) == 0 || receipts[0].AttemptID == attempt {
		return &tracker.NativePhaseContinuation{}, nil
	}
	pinned := receipts[0]
	phases := []tracker.NativePhaseReceipt{
		{Phase: "implement", InputDigest: phaseInputDigest("implement", pinned.HeadSHA, pinned.TreeSHA, lease.PolicyID, reviewState, "")},
	}
	if command != "" {
		phases = append(phases, tracker.NativePhaseReceipt{Phase: "project_gate", InputDigest: phaseInputDigest("project_gate", pinned.HeadSHA, pinned.TreeSHA, lease.PolicyID, reviewState, command)})
	}
	continuation, err := client.ItemPhaseContinuation(ctx, lease.WorkItemID, lease.PolicyID, phases)
	if err != nil {
		return nil, err
	}
	if continuation.LastPushed == nil || *continuation.LastPushed != pinned {
		return nil, errors.New("recorded phase source changed during continuation")
	}
	return &continuation, nil
}

func phaseInputDigest(phase, head, tree, policyID, reviewState, input string) string {
	return policy.Digest([]byte(strings.Join([]string{phase, head, tree, policyID, reviewState, input}, "\x00")))
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
	attemptID, reviewState := e.data.AttemptID, e.stepReviewState()
	e.mu.Unlock()
	digest := phaseInputDigest(phase, head, tree, lease.PolicyID, reviewState, input)
	request := tracker.NativePhaseReceipt{
		Mutation:  tracker.Mutation{IdempotencyKey: attemptID + ":phase:" + digest, LeaseID: lease.ID, FencingToken: lease.FencingToken},
		AttemptID: attemptID, Phase: phase, Branch: branch, HeadSHA: head, TreeSHA: tree, PolicyID: lease.PolicyID, InputDigest: digest,
	}
	client := e.claim.source.NativeClient()
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return err
	}
	recorded, err := client.PublishPhaseReceipt(ctx, lease.WorkItemID, request)
	if nativeTransportUnavailable(err) && ctx.Err() == nil {
		if authorityErr := e.VerifyPhaseAuthority(ctx); authorityErr != nil {
			return errors.Join(err, authorityErr)
		}
		recorded, err = client.PublishPhaseReceipt(ctx, lease.WorkItemID, request)
	}
	if err == nil && recorded != request {
		return errors.New("Hub phase receipt differs from the published source")
	}
	return e.executionError(ctx, err)
}

func (e *nativeExecution) LatestRoundVerdict(ctx context.Context) (*progresscheck.RoundVerdict, error) {
	return e.claim.source.NativeClient().LatestRoundVerdict(ctx, e.claim.lease.WorkItemID)
}

func (e *nativeExecution) RecordRoundVerdict(ctx context.Context, input progresscheck.RoundInput) error {
	if err := e.VerifyPhaseAuthority(ctx); err != nil {
		return err
	}
	if err := e.FlushRuntime(ctx); err != nil {
		return err
	}
	lease := e.claim.lease
	e.mu.Lock()
	attempt := e.data.AttemptID
	e.mu.Unlock()
	request := progresscheck.Publication{
		Mutation:  tracker.Mutation{IdempotencyKey: attempt + ":round:" + input.RoundID, LeaseID: lease.ID, FencingToken: lease.FencingToken},
		AttemptID: attempt, PolicyID: lease.PolicyID, RoundInput: input,
	}
	client := e.claim.source.NativeClient()
	recorded, err := client.PublishRoundVerdict(ctx, lease.WorkItemID, request)
	if nativeTransportUnavailable(err) && ctx.Err() == nil {
		if authorityErr := e.VerifyPhaseAuthority(ctx); authorityErr != nil {
			return errors.Join(err, authorityErr)
		}
		recorded, err = client.PublishRoundVerdict(ctx, lease.WorkItemID, request)
	}
	if err == nil && (recorded.RoundID != input.RoundID || recorded.AttemptID != attempt || recorded.PolicyID != lease.PolicyID || recorded.Action != input.Action || recorded.NextPhase != input.NextPhase || recorded.HeadSHA != input.HeadSHA || recorded.TreeSHA != input.TreeSHA) {
		return errors.New("Hub round verdict differs from the completed round")
	}
	return e.executionError(ctx, err)
}
