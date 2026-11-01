package nativechange

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) PublishValidationEvidence(ctx context.Context, files []runner.ValidationEvidence) error {
	if len(files) == 0 {
		return nil
	}
	if err := e.Validate(ctx); err != nil {
		return err
	}
	content, err := json.Marshal(files)
	if err != nil {
		return err
	}
	return e.claim.source.NativeClient().PublishValidationEvidence(ctx, e.claim.lease.WorkItemID, tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":validation-evidence:" + policy.Digest(content), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken}, files)
}

func (e *nativeExecution) RecordSourceValidation(ctx context.Context, result gate.CommandResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.RecordPipelineTiming(result.PipelineTiming("finalization"))
	e.validation = &result
	return e.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{Validation: &result})
}
