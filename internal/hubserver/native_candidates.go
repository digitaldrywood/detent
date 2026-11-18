package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) nativeClaimAnsweredExclusion(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan, now time.Time) (tracker.NativeAnsweredExclusion, error) {
	if !nativeClaimAnswerRequired(plan) || plan.LatestAttempt.ClaimDisposition == nil {
		return tracker.NativeAnsweredExclusion{}, nil
	}
	switch kind := *plan.LatestAttempt.ClaimDisposition; kind {
	case "human_action", "permission_wait", "recorded_acceptance":
		return tracker.NativeAnsweredExclusion{Kind: kind, AttemptID: plan.LatestAttempt.ID}, nil
	case "":
		if plan.Project.Core.Profile != "native" {
			return tracker.NativeAnsweredExclusion{}, nil
		}
		awaiting, err := s.nativePublishedReviewAwaiting(ctx, scope, plan.Candidate.WorkItemID, plan.LatestAttempt.ID, now)
		if err != nil || !awaiting {
			return tracker.NativeAnsweredExclusion{}, err
		}
		return tracker.NativeAnsweredExclusion{Kind: string(gate.ReasonAutomatedReviewMissing), AttemptID: plan.LatestAttempt.ID}, nil
	}
	return tracker.NativeAnsweredExclusion{}, nil
}

func (s *Service) nativePublishedReviewAwaiting(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, attemptID string, now time.Time) (bool, error) {
	attempt, err := s.readNativeAttemptTenant(ctx, scope, string(item), attemptID)
	if err != nil {
		return false, err
	}
	disposition := attempt.Disposition
	if disposition == nil {
		signal, reported := workpad.SignalFromComment(attempt.CompletionBody, "", string(scope.Project))
		if !reported || signal == nil || signal.Invalid != nil || signal.Fields["completion_kind"] == "operational" {
			return false, nil
		}
		disposition = &tracker.NativeDisposition{Status: signal.Status, Blockers: len(signal.Blockers) != 0, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode}
	}
	if disposition.Status != workpad.StatusComplete || disposition.Blockers || disposition.HumanAction || disposition.ReasonCode != "" || len(disposition.BlockerEvidence) != 0 {
		return false, nil
	}
	source := attempt.Checkpoint
	if source == nil || source.WorktreeState != "clean" && source.WorktreeState != "unpushed" || !hubapi.ValidCommitID(source.HeadSHA) || source.EffectState == "pending" || source.EffectState == "ambiguous" {
		return false, nil
	}
	finalization := attempt.Finalization
	if finalization == nil || !finalization.Changed || finalization.ChangeID == "" || finalization.VersionID == "" || finalization.HeadSHA != source.HeadSHA || finalization.VersionError != "" {
		return false, nil
	}
	detail, err := s.selectedChangeDetail(ctx, scope, string(item), finalization.ChangeID, now)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if detail.Change.CurrentVersion != finalization.VersionID {
		return false, nil
	}
	return changerequest.ReviewState(detail, finalization.VersionID, source.HeadSHA).AwaitingEvidence(), nil
}
