package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) planVerificationRecord(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, request tracker.VerifyFix) (hubrecords.WorkItemRecordPlan, error) {
	if !nativeVerificationAnswerer(scope, plan.Issue) {
		return plan, operatortool.ErrAccessDenied
	}
	if request.ExpectedRevision > 0 && request.ExpectedRevision != plan.Issue.Revision {
		return plan, hubapi.NativeConflict(plan.Issue.Revision)
	}
	if !nativeDoneState(plan.Issue.State) || plan.Issue.Verification == nil || plan.Issue.Verification.State != tracker.VerificationPending {
		return plan, nativeVerificationInvalid("Issue is not awaiting verification")
	}
	now := s.config.now().UTC()
	if strings.TrimSpace(request.Comment) != "" {
		comment, err := s.prepareEvidenceComment(ctx, scope, plan.Issue.WorkItemID, request.Comment, 0)
		if err != nil {
			return plan, err
		}
		plan.VerificationComment = &comment
		attachments, err := s.prepareWorkItemAttachmentReferences(ctx, scope, comment.Plan)
		if err != nil {
			return plan, err
		}
		plan.VerificationAttachments = attachments.Current
	}
	if request.Result == tracker.VerifyResultVerified {
		actor := scope.Actor()
		plan.Issue.Verification = &tracker.NativeVerification{State: tracker.VerificationVerified, VerifiedBy: &actor, VerifiedAt: &now}
	} else {
		if err := hubrecords.ValidateNativeWorkflowTransition(plan.Project.States, plan.Issue.State, nativeVerificationReopenState, scope.Credential.Scope); err != nil {
			return plan, err
		}
		plan.Issue.State = nativeVerificationReopenState
		plan.Issue.Terminal = false
		plan.Issue.ClosedAt = nil
		plan.Issue.Archived = false
		plan.Issue.Verification = nil
	}
	plan.Issue.Revision++
	plan.Issue.UpdatedAt, plan.Issue.LastActivityAt = now, now
	plan.Fields = []string{"verification"}
	plan.EventType = "issue.edited"
	if request.Result == tracker.VerifyResultNotFixed {
		plan.EventType = "workflow.transitioned"
		plan.Fields = append(plan.Fields, "state")
	}
	return plan, nil
}

func (s *Service) writeVerificationComment(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, comment *hubrecords.AttachmentEvidencePublication, attachments []string) error {
	limit, err := s.attachmentQuotaLimit(ctx, w)
	if err != nil {
		return err
	}
	usage, err := w.AdjustCollaborationBytes(ctx, comment.Bytes, limit)
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
	}
	if err := hubrecords.WriteWorkItemRecord(ctx, w, comment.Plan, comment.Fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, comment.Version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, comment.Event); err != nil {
		return err
	}
	for _, id := range attachments {
		if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: id, WorkItemID: comment.Plan.Comment.WorkItemID, CommentID: comment.Plan.Comment.ID}); err != nil {
			return err
		}
	}
	return w.PutSearch(ctx, comment.Search, 0)
}
