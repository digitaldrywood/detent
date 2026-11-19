package claimapi

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func NativeAttemptClaimDisposition(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint, project tracker.ProjectID) string {
	if data.Disposition == nil {
		if signal, reported := workpad.SignalFromComment(data.CompletionBody, "", string(project)); reported && signal != nil && signal.Invalid == nil && (signal.HumanAction != "" || signal.ReasonCode == "permission_wait") {
			data.Disposition = &tracker.NativeDisposition{Status: signal.Status, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode}
		}
	}
	if kind := AnsweredClaimKind(data, checkpoint); kind != "" {
		return kind
	}
	if data.TerminalFailure != nil {
		return "progress"
	}
	if data.Disposition != nil && (data.Disposition.Status != workpad.StatusComplete || data.Disposition.Blockers || data.Disposition.ReasonCode != "") {
		return "progress"
	}
	if checkpoint != nil && (checkpoint.ExternalEffect == "git_push" || checkpoint.ExternalEffect == "pr_create") && (data.Disposition == nil || checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") {
		return "progress"
	}
	return ""
}

func AttemptClaimFacts(attempt leasestore.Attempt, disposition string, unresolvedOnly bool) leasestore.AttemptClaimFacts {
	return leasestore.AttemptClaimFacts{AttemptID: attempt.ID, Sequence: attempt.Sequence, Status: attempt.Status, PayloadObjectKey: attempt.PayloadObjectKey, CheckpointObjectKey: attempt.CheckpointObjectKey, Disposition: disposition, UnresolvedOnly: unresolvedOnly}
}

func (d AttemptFactsDeps) PrepareAttemptClaimFacts(ctx context.Context, snapshot hubrecords.TenantAttemptSnapshot) (leasestore.AttemptClaimFacts, error) {
	run, err := d.ReadAttempt(ctx, snapshot)
	if err != nil {
		return leasestore.AttemptClaimFacts{}, err
	}
	if err := hubrecords.NativeAttemptRunMismatch(snapshot, run); err != nil {
		return leasestore.AttemptClaimFacts{}, fmt.Errorf("recover attempt claim facts: %w", err)
	}
	status, statusErr := d.AttemptStatus(tracker.NativeRunEvent{Type: snapshot.Event.Type, Data: run})
	if statusErr != nil || status != snapshot.Attempt.Status {
		return leasestore.AttemptClaimFacts{}, fmt.Errorf("recover attempt claim status: %w", artifact.ErrIntegrity)
	}
	checkpoint, err := d.ReadCheckpoint(ctx, snapshot)
	if err != nil {
		return leasestore.AttemptClaimFacts{}, err
	}
	return AttemptClaimFacts(snapshot.Attempt, NativeAttemptClaimDisposition(run, checkpoint, snapshot.Event.ProjectID), true), nil
}
