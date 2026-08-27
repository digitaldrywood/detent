package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
)

type laneMutationDisposition = store.LaneMutationDisposition

const (
	laneMutationPreserveOwnership = store.LaneMutationPreserveOwnership
	laneMutationAcceptCompletion  = store.LaneMutationAcceptCompletion
	laneMutationRevokeWorker      = store.LaneMutationRevokeWorker
)

var errLaneMutationDispositionRequired = errors.New("live worker lease requires a lane mutation disposition")

func (o *Orchestrator) prepareLaneMutation(
	ctx context.Context,
	state *State,
	issueID string,
	issue connector.Issue,
	targetState string,
	at time.Time,
	reason string,
	dispositions []laneMutationDisposition,
) (store.LaneMutationReceipt, Running, bool, error) {
	if state == nil {
		return store.LaneMutationReceipt{}, Running{}, false, nil
	}
	issueID = strings.TrimSpace(issueID)
	running, leased := state.Running[issueID]
	if !leased {
		return store.LaneMutationReceipt{}, Running{}, false, nil
	}
	if len(dispositions) != 1 {
		return store.LaneMutationReceipt{}, running, true, errLaneMutationDispositionRequired
	}
	disposition := dispositions[0]
	switch disposition {
	case laneMutationPreserveOwnership, laneMutationAcceptCompletion, laneMutationRevokeWorker:
	default:
		return store.LaneMutationReceipt{}, running, true, fmt.Errorf("invalid live worker lane mutation disposition %q", disposition)
	}
	if running.WorkAttemptID <= 0 || running.Generation == 0 {
		return store.LaneMutationReceipt{}, running, true, errors.New("live worker lane mutation requires a work-attempt ID and generation")
	}
	if o.laneMutations == nil {
		return store.LaneMutationReceipt{}, running, true, errors.New("live worker lane mutation receipt store is unavailable")
	}
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	fromState := strings.TrimSpace(running.Issue.State)
	if fromState == "" {
		fromState = strings.TrimSpace(issue.State)
	}
	receipt, err := o.laneMutations.BeginLaneMutation(context.WithoutCancel(ctx), store.LaneMutationStart{
		ProjectID:     o.workflowMetricsProjectID(),
		IssueID:       issueID,
		WorkAttemptID: running.WorkAttemptID,
		Generation:    running.Generation,
		Disposition:   disposition,
		FromState:     fromState,
		ToState:       strings.TrimSpace(targetState),
		Reason:        strings.TrimSpace(reason),
		RequestedAt:   at.UTC(),
	})
	if err != nil {
		return store.LaneMutationReceipt{}, running, true, fmt.Errorf("persist live worker lane mutation receipt: %w", err)
	}
	return receipt, running, true, nil
}

func (o *Orchestrator) resolveLaneMutation(
	ctx context.Context,
	receipt store.LaneMutationReceipt,
	result store.LaneMutationTrackerResult,
	at time.Time,
	mutationErr error,
) error {
	if receipt.ID <= 0 || o.laneMutations == nil {
		return nil
	}
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	errMessage := ""
	if mutationErr != nil {
		errMessage = mutationErr.Error()
	}
	if err := o.laneMutations.ResolveLaneMutation(context.WithoutCancel(ctx), store.LaneMutationResolution{
		ReceiptID:     receipt.ID,
		WorkAttemptID: receipt.WorkAttemptID,
		Generation:    receipt.Generation,
		TrackerResult: result,
		ResolvedAt:    at.UTC(),
		ErrorMessage:  errMessage,
	}); err != nil {
		return fmt.Errorf("resolve live worker lane mutation receipt: %w", err)
	}
	return nil
}

func (o *Orchestrator) applyLaneMutationDisposition(
	ctx context.Context,
	state *State,
	running Running,
	receipt store.LaneMutationReceipt,
	issue connector.Issue,
	at time.Time,
) {
	if at.IsZero() {
		at = receipt.ResolvedAt
	}
	if at.IsZero() {
		at = receipt.RequestedAt
	}
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	transitioned := mergeIssueTrackerFields(running.Issue, issue)
	applyIssueStateSnapshot(&transitioned, receipt.ToState, at)
	receipt.TrackerResult = store.LaneMutationTrackerApplied
	receipt.ResolvedAt = at.UTC()
	switch receipt.Disposition {
	case laneMutationPreserveOwnership, laneMutationAcceptCompletion:
		running.Issue = transitioned
		running.laneMutation = receipt
		state.Running[receipt.IssueID] = running
		if claimed, ok := state.Claimed[receipt.IssueID]; ok {
			claimed.Issue = cloneIssue(transitioned)
			state.Claimed[receipt.IssueID] = claimed
		}
	case laneMutationRevokeWorker:
		o.beginLaneRevocationForMutation(ctx, state, running, transitioned, at, receipt)
	}
}

func (o *Orchestrator) laneMutationReceipt(
	ctx context.Context,
	running Running,
	toState string,
) (store.LaneMutationReceipt, bool, error) {
	if runningLaneMutationMatches(running, connector.Issue{State: toState}) {
		return running.laneMutation, true, nil
	}
	if o.laneMutations == nil || running.WorkAttemptID <= 0 || running.Generation == 0 || strings.TrimSpace(running.Issue.ID) == "" {
		return store.LaneMutationReceipt{}, false, nil
	}
	receipt, err := o.laneMutations.LaneMutationReceipt(context.WithoutCancel(ctx), store.LaneMutationLookup{
		ProjectID:     o.workflowMetricsProjectID(),
		IssueID:       running.Issue.ID,
		WorkAttemptID: running.WorkAttemptID,
		Generation:    running.Generation,
		ToState:       toState,
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.LaneMutationReceipt{}, false, nil
	}
	if err != nil {
		return store.LaneMutationReceipt{}, false, fmt.Errorf("load live worker lane mutation receipt: %w", err)
	}
	if receipt.IssueID != strings.TrimSpace(running.Issue.ID) ||
		receipt.WorkAttemptID != running.WorkAttemptID ||
		receipt.Generation != running.Generation ||
		normalizeState(receipt.ToState) != normalizeState(toState) {
		return store.LaneMutationReceipt{}, false, errors.New("live worker lane mutation receipt does not match the current owner")
	}
	return receipt, true, nil
}

func (o *Orchestrator) consumeLaneMutationReceipt(
	ctx context.Context,
	receipt store.LaneMutationReceipt,
	running Running,
	toState string,
	at time.Time,
) (store.LaneMutationReceipt, error) {
	if o.laneMutations == nil {
		return store.LaneMutationReceipt{}, errors.New("live worker lane mutation receipt store is unavailable")
	}
	if receipt.IssueID != strings.TrimSpace(running.Issue.ID) ||
		receipt.WorkAttemptID != running.WorkAttemptID ||
		receipt.Generation != running.Generation ||
		normalizeState(receipt.ToState) != normalizeState(toState) {
		return store.LaneMutationReceipt{}, errors.New("live worker lane mutation receipt does not match the current owner")
	}
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	consumed, err := o.laneMutations.ConsumeLaneMutation(context.WithoutCancel(ctx), store.LaneMutationConsumption{
		ReceiptID:     receipt.ID,
		ProjectID:     receipt.ProjectID,
		IssueID:       receipt.IssueID,
		WorkAttemptID: receipt.WorkAttemptID,
		Generation:    receipt.Generation,
		ToState:       toState,
		ConsumedAt:    at.UTC(),
	})
	if err != nil {
		return store.LaneMutationReceipt{}, fmt.Errorf("consume live worker lane mutation receipt: %w", err)
	}
	if consumed.ID != receipt.ID || consumed.Disposition != receipt.Disposition {
		return store.LaneMutationReceipt{}, errors.New("consumed live worker lane mutation receipt changed identity")
	}
	return consumed, nil
}

func runningLaneMutationMatches(running Running, issue connector.Issue) bool {
	receipt := running.laneMutation
	return receipt.ID > 0 &&
		receipt.IssueID == strings.TrimSpace(running.Issue.ID) &&
		receipt.WorkAttemptID == running.WorkAttemptID &&
		receipt.Generation == running.Generation &&
		normalizeState(receipt.ToState) == normalizeState(issue.State)
}
