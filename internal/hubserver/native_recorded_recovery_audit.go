package hubserver

import (
	"context"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (s *Service) recoverRecordedAuditHold(ctx context.Context, scope hubscope.NativeScope, clearance releaseaudits.AuditHoldClearance) error {
	err := s.applyRecordedAuditRecovery(ctx, scope, clearance)
	if errors.Is(err, ErrNoClaimableWork) || recordedRecoveryIneligible(err) {
		return nil
	}
	return err
}

func (s *Service) applyRecordedAuditRecovery(ctx context.Context, scope hubscope.NativeScope, clearance releaseaudits.AuditHoldClearance) error {
	row, version, err := s.readWorkItemRecord(ctx, scope, string(clearance.WorkItem))
	if err != nil {
		return err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return err
	}
	recovery, err := s.planRecordedRecoveryTenant(ctx, scope, row, issue, "", true, &clearance)
	if err != nil {
		return err
	}
	scope.SourceActor = &tracker.Actor{Kind: "system", PrincipalID: "recorded_recovery"}
	write, err := s.prepareNativeRecordedRecoveryWrite(ctx, scope, recovery, &clearance)
	if err != nil {
		return err
	}
	if err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return write(ctx, w)
	}); err != nil {
		return err
	}
	s.signalNativeDispatch(scope)
	return nil
}

func recordedRecoveryIneligible(err error) bool {
	var failure *hubapi.NativeError
	return errors.Is(err, corestore.ErrNotFound) || errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, tracker.ErrStaleFencingToken) ||
		errors.Is(err, artifact.ErrMissing) || errors.Is(err, artifact.ErrIntegrity) || errors.As(err, &failure) && failure.Code == "not_found"
}

func recordedAuditHold(clearance releaseaudits.AuditHoldClearance, transition tracker.CollaborationEvent, attempt tracker.NativeAttempt) bool {
	if clearance.VersionCreatedAt.After(transition.RecordedAt) || clearance.ReviewCreatedAt.After(transition.RecordedAt) ||
		transition.Data.Change != nil && *transition.Data.Change != clearance.Source || attempt.Status == "cancelled" || attempt.Status == "interrupted" ||
		attempt.Disposition != nil && attempt.Disposition.ReasonCode == "permission_wait" {
		return false
	}
	return matchesRecordedAuditQuestion(transition.Data.ReasonDetail, clearance.HumanAction) &&
		(!typedWorkerHumanAction(attempt.CompletionBody) || matchesRecordedAuditQuestion(attempt.CompletionBody, clearance.HumanAction))
}

type recordedAuditQuestion struct {
	action string
	runner string
	proof  string
}

func auditQuestion(clearance releaseaudits.AuditHoldClearance, runner string) recordedAuditQuestion {
	return recordedAuditQuestion{action: clearance.HumanAction, runner: runner, proof: clearance.Source.ChangeID + "/" + strconv.FormatInt(clearance.ReviewSequence, 10)}
}

func (q recordedAuditQuestion) matchesComment(row corestore.Comment, body string) bool {
	return q.runner != "" && row.ActorKind == "runner" && row.ActorPrincipalID == q.runner &&
		(row.EditedByKind == nil || *row.EditedByKind == "runner" && hubapi.StringValue(row.EditedByPrincipalID) == q.runner) &&
		matchesRecordedAuditQuestion(body, q.action)
}

func matchesRecordedAuditQuestion(text, action string) bool {
	block, found := workpad.LastStatusBlock(text)
	if !found || action == "" {
		return false
	}
	signal, err := workpad.ParseStatusBlock(block, "")
	return err == nil && signal.Invalid == nil && signal.Status == workpad.StatusBlocked && signal.HumanAction == action &&
		len(signal.Blockers) == 0 && signal.ReasonCode != "permission_wait" && signal.Fields["human_action_source"] == ""
}
