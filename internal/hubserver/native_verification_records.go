package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) planVerificationRecord(ctx context.Context, scope nativeScope, plan workItemRecordPlan, request tracker.VerifyFix) (workItemRecordPlan, error) {
	if !nativeVerificationAnswerer(scope, plan.issue) {
		return plan, operatortool.ErrAccessDenied
	}
	if request.ExpectedRevision > 0 && request.ExpectedRevision != plan.issue.Revision {
		return plan, nativeConflict(plan.issue.Revision)
	}
	if !nativeDoneState(plan.issue.State) || plan.issue.Verification == nil || plan.issue.Verification.State != tracker.VerificationPending {
		return plan, nativeVerificationInvalid("Issue is not awaiting verification")
	}
	now := s.config.now().UTC()
	if strings.TrimSpace(request.Comment) != "" {
		comment, err := s.prepareEvidenceComment(ctx, scope, plan.issue.WorkItemID, request.Comment, 0)
		if err != nil {
			return plan, err
		}
		plan.verificationComment = &comment
		plan.verificationAttachments, err = s.prepareWorkItemAttachmentReferences(ctx, scope, comment.plan)
		if err != nil {
			return plan, err
		}
	}
	if request.Result == tracker.VerifyResultVerified {
		actor := scope.actor()
		plan.issue.Verification = &tracker.NativeVerification{State: tracker.VerificationVerified, VerifiedBy: &actor, VerifiedAt: &now}
	} else {
		if err := validateNativeWorkflowTransition(plan.project.States, plan.issue.State, nativeVerificationReopenState, scope.credential.Scope); err != nil {
			return plan, err
		}
		plan.issue.State = nativeVerificationReopenState
		plan.issue.Archived = false
		plan.issue.Verification = nil
	}
	plan.issue.Revision++
	plan.issue.UpdatedAt, plan.issue.LastActivityAt = now, now
	plan.fields = []string{"verification"}
	plan.eventType = "issue.edited"
	if request.Result == tracker.VerifyResultNotFixed {
		plan.eventType = "workflow.transitioned"
		plan.fields = append(plan.fields, "state")
	}
	return plan, nil
}

func (s *Service) writeVerificationComment(ctx context.Context, w tenantstore.Writer, scope nativeScope, comment *attachmentEvidencePublication, attachments []string) error {
	limit, err := s.attachmentQuotaLimit(ctx, w)
	if err != nil {
		return err
	}
	usage, err := w.AdjustCollaborationBytes(ctx, comment.bytes, limit)
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
	}
	if err := writeWorkItemRecord(ctx, w, comment.plan, comment.fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, comment.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, comment.event); err != nil {
		return err
	}
	for _, id := range attachments {
		if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.project, AttachmentID: id, WorkItemID: comment.plan.comment.WorkItemID, CommentID: comment.plan.comment.ID}); err != nil {
			return err
		}
	}
	return w.PutSearch(ctx, comment.search, 0)
}
