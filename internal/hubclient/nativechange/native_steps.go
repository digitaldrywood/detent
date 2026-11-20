package nativechange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/evidence"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
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
	receipt.ReviewState = e.stepReviewStateFor(receipt.Step)
	if receipt.Step != "plan" {
		receipt.ReviewEpoch = e.stepReviewEpoch()
	}
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
	binding.PolicyID, binding.ReviewState = e.data.PolicyID, e.stepReviewStateFor(binding.Step)
	if binding.Step != "plan" {
		binding.ReviewEpoch = e.stepReviewEpoch()
	}
	for _, receipt := range e.data.Steps {
		if receipt.Matches(binding) {
			return e.restoreStep(ctx, receipt)
		}
	}
	attempts := e.claim.recovery.Attempts
	if binding.Step == "plan" && e.phaseSource != nil {
		owner := e.phaseSource
		if !slices.ContainsFunc(attempts, func(attempt tracker.NativeAttempt) bool { return attempt.AttemptID == owner.AttemptID }) {
			attempt, err := e.claim.source.NativeClient().NativeAttempt(ctx, e.claim.lease.WorkItemID, owner.AttemptID)
			if err != nil {
				return tracker.NativeStepReceipt{}, fmt.Errorf("read plan receipt attempt: %w", err)
			}
			if attempt.AttemptID != owner.AttemptID || attempt.LeaseID != owner.LeaseID ||
				attempt.FencingToken != owner.FencingToken || attempt.PolicyID != owner.PolicyID {
				return tracker.NativeStepReceipt{}, errors.New("plan receipt does not match its attempt")
			}
			attempts = append(slices.Clone(attempts), attempt)
		}
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		if binding.Step == "plan" && (e.phaseSource == nil || e.phaseSource.AttemptID != attempt.AttemptID) {
			continue
		}
		for _, receipt := range attempt.Steps {
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

func (e *nativeExecution) stepReviewStateFor(step string) string {
	if step == "plan" {
		return planIssueDigest(e.Recovery())
	}
	return e.stepReviewState()
}

func planIssueDigest(recovery tracker.NativeRecovery) string {
	return tracker.NativeStepReviewState(tracker.NativeRecovery{Issue: recovery.Issue})
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
	project, err := client.Project(ctx)
	if err != nil {
		return nil, fmt.Errorf("read phase publication repository: %w", err)
	}
	e.mu.Lock()
	attempt, reviewState := e.data.AttemptID, e.stepReviewState()
	sameAttempt := tracker.NativePhaseContinuation{RepositoryURL: project.CloneURL}
	if len(receipts) > 0 {
		e.phaseSource = &receipts[0]
		if landing := e.pendingLandingResolution(); landing != nil && e.role == runner.RoleMerge &&
			receipts[0].AttemptID == attempt && receipts[0].HeadSHA == landing.HeadSHA && receipts[0].PolicyID == lease.PolicyID {
			sameAttempt.LastPushed = &receipts[0]
		}
	}
	e.mu.Unlock()
	gates := e.scheduler.host.PinnedExecutionPolicy(string(lease.WorkItemID)).Gates
	recovery := e.Recovery()
	var workflow []tracker.CollaborationEvent
	if gates.PlanEnabled && (len(receipts) == 0 && !strings.EqualFold(recovery.Issue.State, "Todo") && recovery.Change == nil || len(receipts) != 0 && receipts[0].Phase == "plan_draft") {
		workflow, err = client.RecentWorkflowTransitions(ctx, lease.WorkItemID, 3)
		if err != nil {
			return nil, fmt.Errorf("read planning workflow transitions: %w", err)
		}
	}
	if len(receipts) == 0 || receipts[0].AttemptID == attempt {
		sameAttempt.Workflow = workflow
		return &sameAttempt, nil
	}
	pinned := receipts[0]
	var phases []tracker.NativePhaseReceipt
	requirePhase := func(phase, input string) {
		state := reviewState
		if planPhase(phase) {
			state = planIssueDigest(e.Recovery())
		}
		phases = append(phases, tracker.NativePhaseReceipt{
			Phase: phase, InputDigest: phaseInputDigest(phase, pinned.HeadSHA, pinned.TreeSHA, lease.PolicyID, state, input),
		})
	}
	if gates.PlanEnabled && planPhase(pinned.Phase) {
		if gates.PlanReview == gate.PlanReviewHuman {
			requirePhase("plan_draft", "")
		}
		requirePhase("plan_review", "")
		requirePhase("plan", "")
	}
	requirePhase("implement", "")
	if command != "" {
		requirePhase("project_gate", command)
	}
	version := ""
	if current := e.claim.recovery.Change; current != nil && current.HeadSHA == pinned.HeadSHA {
		version = current.VersionID
	}
	if gates.SecurityAudit {
		requirePhase("security_audit", "")
	}
	if gates.AutomatedReview == gate.AutomatedReviewRequired {
		requirePhase("implementation_review", version)
	}
	if gates.Validator {
		requirePhase("acceptance", version)
	}
	requirePhase("merge", version)
	continuation, err := client.ItemPhaseContinuation(ctx, lease.WorkItemID, lease.PolicyID, phases)
	if err != nil {
		return nil, err
	}
	if continuation.LastPushed == nil || *continuation.LastPushed != pinned {
		return nil, errors.New("recorded phase source changed during continuation")
	}
	continuation.RepositoryURL = project.CloneURL
	continuation.Workflow = workflow
	return &continuation, nil
}

func planPhase(phase string) bool {
	return phase == "plan" || phase == "plan_draft" || phase == "plan_review"
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
	if planPhase(phase) {
		reviewState = planIssueDigest(e.Recovery())
	}
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
	if input.Action == progresscheck.RoundDone && input.Phase == "acceptance" {
		if err := e.VerifyPhaseAuthority(ctx); err != nil {
			return err
		}
		e.mu.Lock()
		e.preparedRoundVerdict = &input
		e.mu.Unlock()
		return nil
	}
	return e.publishRoundVerdict(ctx, input)
}

func (e *nativeExecution) publishAcceptedNoChangeVerdict(ctx context.Context) error {
	e.mu.Lock()
	input, runtime, finalization := e.preparedRoundVerdict, e.data.Runtime, e.data.Finalization
	accepted := e.data.Outcome == "succeeded" && input != nil && runtime != nil && runtime.Completion != nil && runtime.Completion.AcceptanceRecorded &&
		finalization != nil && finalization.NoChange != nil && finalization.NoChange.VerifiedSHA == input.HeadSHA
	e.mu.Unlock()
	if !accepted {
		return nil
	}
	if err := e.publishRoundVerdict(ctx, *input); err != nil {
		return err
	}
	e.mu.Lock()
	if e.preparedRoundVerdict == input {
		e.preparedRoundVerdict = nil
	}
	e.mu.Unlock()
	return nil
}

func (e *nativeExecution) publishRoundVerdict(ctx context.Context, input progresscheck.RoundInput) error {
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
	err = e.executionError(ctx, err)
	var refusal *APIError
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) && errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
		switch refusal.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		default:
			return errors.Join(progresscheck.ErrRoundVerdictRejected, err)
		}
	}
	return err
}
