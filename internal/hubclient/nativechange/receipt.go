package nativechange

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/hubclient/publication"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) fencedAttempt(ctx context.Context) (tracker.NativeAttempt, error) {
	client := e.claim.source.NativeClient()
	attempt, err := client.ExecutionAttempt(ctx, e.claim.lease.WorkItemID, e.data.AttemptID)
	if err != nil {
		return attempt, err
	}
	if attempt.LeaseID != e.claim.lease.ID || attempt.FencingToken != e.claim.lease.FencingToken || attempt.AttemptID != e.data.AttemptID || attempt.RunID != e.data.RunID || attempt.PolicyID != e.data.PolicyID || !reflect.DeepEqual(attempt.Identity, e.data.Identity) {
		return attempt, runner.ErrExecutionAuthorityUnavailable
	}
	return attempt, nil
}

func (e *nativeExecution) sendGeneration(ctx context.Context, diff *tracker.AttemptDiffRequest, authorize func(context.Context) error) error {
	if e.submittedDiff == nil || e.submittedDiff.Generation != diff.Generation {
		request := *diff
		e.submittedDiff = &request
	}
	client := e.claim.source.NativeClient()
	request := e.submittedDiff
	_, err := client.PostExecutionDiff(ctx, e.data.AttemptID, request, authorize)
	var refusal *APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict || refusal.Code != "stale_generation" {
		return err
	}
	attempt, readErr := e.fencedAttempt(ctx)
	if readErr != nil {
		return e.executionError(ctx, readErr)
	}
	if attempt.ClaimReleasedAt != nil || attempt.Outcome != "" {
		return runner.ErrExecutionAuthorityUnavailable
	}
	stored, readErr := client.ExecutionDiff(ctx, e.data.AttemptID, request.Generation.Seq)
	if readErr != nil {
		if errors.As(readErr, &refusal) && refusal.Status == http.StatusNotFound && refusal.Code == "not_found" {
			return errors.Join(errUnverifiedReceipt, readErr)
		}
		readErr = e.executionError(ctx, readErr)
		if errors.Is(api.MutationEffectError(ctx), runner.ErrOperatorStopped) {
			return receiptProofError(ctx, readErr)
		}
		return readErr
	}
	producer := request.Producer
	producer.RunnerID = attempt.RunnerID
	if producer.RunnerID == "" || stored.AttemptID != attempt.AttemptID || stored.Producer != producer || stored.Generation != request.Generation || stored.BaseSHA != request.BaseSHA || stored.HeadSHA != request.HeadSHA || !reflect.DeepEqual(stored.Files, request.Files) {
		return errors.Join(errUnverifiedReceipt, err)
	}
	return nil
}

type completedReceipt struct {
	gate              *tracker.NativeStepReceipt
	validation        *gate.CommandResult
	previous          *tracker.NativeCheckpoint
	verifyPublication func(context.Context) error
	standalone        *tracker.NativeRunEvent
}

var errUnverifiedReceipt = errors.New("completed receipt source cannot be verified")

func receiptProofError(ctx context.Context, err error) error {
	if ctx.Err() != nil || nativeAuthorityLost(err) || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		return err
	}
	return errors.Join(errUnverifiedReceipt, err)
}

func validationRuntime(current *tracker.NativeRuntimeObservation, result gate.CommandResult, at time.Time) *tracker.NativeRuntimeObservation {
	runtime := tracker.NativeRuntimeObservation{Phase: "validation", HeartbeatAt: at.UTC()}
	if current != nil {
		runtime = *current
	}
	runtime.Validation = &result
	return &runtime
}

func (e *nativeExecution) executionCause(ctx context.Context) error {
	cause := api.MutationEffectError(ctx)
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	if e.workerCause != nil {
		worker := e.workerCause()
		if errors.Is(worker, runner.ErrExecutionAuthorityUnavailable) || errors.Is(worker, runner.ErrOperatorStopped) {
			return errors.Join(cause, worker)
		}
	}
	return cause
}

func (e *nativeExecution) receiptAuthority(ctx context.Context) error {
	if e.leaseValidity() <= 0 {
		return runner.ErrExecutionAuthorityUnavailable
	}
	cause := e.executionCause(ctx)
	if ctx.Err() != nil || errors.Is(cause, runner.ErrExecutionAuthorityUnavailable) || !errors.Is(cause, runner.ErrOperatorStopped) {
		return cause
	}
	return nil
}

func durableReceipt(attempt *tracker.NativeAttempt, event *tracker.NativeRunEvent) bool {
	return event != nil && attempt.Sequence >= event.Data.Sequence && reflect.DeepEqual(attempt.Checkpoint, event.Data.Handoff) && reflect.DeepEqual(attempt.Steps, event.Data.Steps)
}

func (e *nativeExecution) deliverStoppedReceipt(ctx context.Context, attempt *tracker.NativeAttempt) error {
	r, original := e.receipt, e.pending
	if r == nil {
		return nil
	}
	if err := e.receiptAuthority(ctx); err != nil {
		return err
	}
	if original == nil || original.Type != "run.checkpointed" || !checkpoint.SameRun(original.Data, attempt.NativeRunData) || attempt.ClaimReleasedAt != nil || attempt.Outcome != "" {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if !durableReceipt(attempt, original) && !durableReceipt(attempt, r.standalone) {
		if r.verifyPublication != nil && (r.previous == nil || r.previous.EffectState != "pending" || !reflect.DeepEqual(attempt.Checkpoint, r.previous)) {
			return errUnverifiedReceipt
		}
		r.prepareStandalone(attempt, original)
		generation := r.standalone.Data.Sequence
		if e.generationDiff != nil && e.generationDiff.Generation.Seq == original.Data.Sequence {
			generation = original.Data.Sequence
		}
		if err := e.captureGeneration(ctx, generation); err != nil {
			return receiptProofError(ctx, err)
		}
		diff := e.generationDiff
		if e.storedSeq != diff.Generation.Seq {
			if err := e.sendGeneration(ctx, diff, e.verifyReceipt); err != nil {
				return err
			}
			e.storedSeq = diff.Generation.Seq
		}
		if err := e.claim.source.NativeClient().AppendExecutionEvent(ctx, e.claim.lease.WorkItemID, *r.standalone, e.verifyReceipt); err != nil {
			return e.executionError(ctx, err)
		}
		attempt.NativeRunData = r.standalone.Data
		attempt.Checkpoint = r.standalone.Data.Handoff
	}
	e.data = attempt.NativeRunData
	e.data.Handoff = attempt.Checkpoint
	e.pending, e.receipt, e.observations = nil, nil, nil
	return nil
}

func (e *nativeExecution) stopAttempt(ctx context.Context) (*tracker.NativeAttempt, error) {
	if !e.claim.lease.ExecutionAttemptView {
		return nil, e.receiptAuthority(ctx)
	}
	var released *tracker.NativeAttempt
	var interruption tracker.NativeRunEvent
	var conflict error
	var contested int64
	err := api.ReconcileTerminal(ctx, func() (bool, error) {
		attempt, err := e.fencedAttempt(ctx)
		if err != nil {
			pendingStart := e.data.Sequence == 0 && e.pending != nil && e.pending.Type == "run.started" && e.pending.Data.Sequence == 1
			var refusal *APIError
			if pendingStart && errors.As(err, &refusal) && refusal.Status == http.StatusNotFound && refusal.Code == "not_found" {
				if err := e.receiptAuthority(ctx); err != nil {
					return false, err
				}
				_, err = e.claim.source.NativeClient().ValidateLeaseOwner(ctx, e.claim.lease)
			}
			err = e.executionError(ctx, err)
			return nativeTransportUnavailable(err), err
		}
		if attempt.ClaimReleasedAt != nil {
			released = &attempt
			return false, nil
		}
		if err := e.receiptAuthority(ctx); err != nil {
			return false, err
		}
		if attempt.Outcome == "" {
			if conflict != nil && attempt.Sequence <= contested {
				return false, conflict
			}
			before := attempt.Sequence
			if interruption.Data.Sequence != before+1 {
				err = e.deliverStoppedReceipt(ctx, &attempt)
				if errors.Is(err, errUnverifiedReceipt) {
					err = nil
				}
				if err == nil {
					interruption = interruptedRunEvent(attempt.NativeRunData)
					before = attempt.Sequence
				}
			}
			if err == nil {
				err = e.claim.source.NativeClient().AppendExecutionEvent(ctx, e.claim.lease.WorkItemID, interruption, e.receiptAuthority)
			}
			if err != nil {
				var refusal *APIError
				if errors.As(err, &refusal) && refusal.Status == http.StatusConflict && refusal.Code == "run_sequence_conflict" {
					conflict, contested = err, before
					return true, err
				}
				return nativeTransportUnavailable(err), e.executionError(ctx, err)
			}
			attempt.NativeRunData = interruption.Data
		}
		e.data, e.pending, e.receipt, e.observations = attempt.NativeRunData, nil, nil, nil
		return false, nil
	})
	return released, err
}

func (r *completedReceipt) prepareStandalone(attempt *tracker.NativeAttempt, original *tracker.NativeRunEvent) {
	if r.standalone == nil || r.standalone.Data.Sequence != attempt.Sequence+1 {
		data := executionEventData("run.checkpointed", attempt.NativeRunData)
		data.Sequence++
		data.Handoff = original.Data.Handoff
		if r.gate != nil {
			data.Steps = withStep(data.Steps, *r.gate)
			data.Handoff = attempt.Checkpoint
			if data.Handoff == nil {
				data.Handoff = &r.gate.Source
			}
			if r.validation.TimingStage == "finalization" {
				data.Runtime = validationRuntime(data.Runtime, tracker.PublicGateResult(*r.validation), r.gate.CompletedAt)
			}
		}
		r.standalone = &tracker.NativeRunEvent{Type: "run.checkpointed", SchemaVersion: 1, Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":stopped-receipt:" + strconv.FormatInt(data.Sequence, 10)}, Data: data}
	}
}

func (e *nativeExecution) verifyReceipt(ctx context.Context) error {
	r, diff := e.receipt, e.generationDiff
	if err := e.receiptAuthority(ctx); err != nil {
		return err
	}
	if r.verifyPublication != nil {
		if r.previous.HeadSHA != diff.HeadSHA {
			return errUnverifiedReceipt
		}
		if err := r.verifyPublication(ctx); errors.Is(err, publication.ErrSourceChanged) {
			return errors.Join(errUnverifiedReceipt, err)
		} else if err != nil {
			return receiptProofError(ctx, err)
		}
	} else {
		if e.diffSource == nil || r.gate.Source.WorkspaceDigest == "" {
			return errUnverifiedReceipt
		}
		live, ok := e.diffSource(runner.VerifyDiffSource(ctx))
		if !ok || live.WorkspaceDigest != r.gate.Source.WorkspaceDigest || live.HeadSHA != r.gate.Source.HeadSHA || live.HeadSHA != diff.HeadSHA || live.BaseSHA != diff.BaseSHA || !reflect.DeepEqual(live.Files, diff.Files) {
			return errUnverifiedReceipt
		}
	}
	return e.receiptAuthority(ctx)
}

func interruptedRunEvent(current tracker.NativeRunData) tracker.NativeRunEvent {
	current.Outcome = "interrupted"
	data := executionEventData("run.finished", current)
	data.Sequence++
	if data.Runtime != nil {
		runtime := *data.Runtime
		runtime.Phase = "completed"
		data.Runtime = &runtime
	}
	return tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":interrupted:" + strconv.FormatInt(data.Sequence, 10)}, Data: data}
}
