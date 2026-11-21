package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) reportNoChangeChecklistGate(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, attempt *tracker.NativeAttempt, list *tracker.ExecutionChecklist) {
	if attempt.Status != "succeeded" || attempt.Finalization == nil || attempt.Finalization.NoChange == nil {
		return
	}
	completion := attempt.Finalization.NoChange
	if completion.PolicyID != list.PolicyID || completion.PolicyID != attempt.PolicyID || completion.VerifiedSHA == "" {
		return
	}
	version := completion.VersionID()
	for i := range list.Checks {
		check := &list.Checks[i]
		if check.Kind != tracker.CheckProjectGate || check.Status != tracker.CheckUnverified {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
		defer cancel()
		for j := len(attempt.Steps) - 1; j >= 0; j-- {
			step := attempt.Steps[j]
			if (step.Step != "review" && step.Step != "validator") || step.PolicyID != completion.PolicyID || step.Change.ChangeID != "" ||
				step.Change.VersionID != version || step.Change.HeadSHA != completion.VerifiedSHA || step.Source.HeadSHA != completion.VerifiedSHA {
				continue
			}
			step, err := s.readChecklistReviewStep(ctx, scope, item, attempt, step)
			if err != nil {
				s.config.Logger.WarnContext(ctx, "read no-change checklist review", "attempt_id", attempt.AttemptID, "error", err)
				continue
			}
			if command := progresscheck.NoChangeChecklistGate(*check, *completion, step); command != nil {
				check.Status, check.Head, check.Tree, check.Receipt, check.CheckedAt = tracker.CheckPassed, command.HeadSHA, command.TreeSHA, command.ReceiptID, step.CompletedAt
				return
			}
		}
	}
}

func (s *Service) readChecklistReviewStep(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, attempt *tracker.NativeAttempt, step tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	if step.PayloadArtifact == nil && step.PayloadAttachment == nil {
		return step, nil
	}
	var raw []byte
	var err error
	if ref := step.PayloadArtifact; ref != nil {
		if step.PayloadAttachment != nil || ref.Validate() != nil || ref.OrganizationID != string(scope.Organization) || ref.ProjectID != string(scope.Project) ||
			ref.WorkItemID != string(item) || ref.AttemptID != attempt.AttemptID || ref.RunID != attempt.RunID ||
			ref.Kind != "log" || ref.State != "complete" || ref.Availability != "available" {
			return step, artifact.ErrIntegrity
		}
		grant, err := s.artifactReadGrantTenant(ctx, scope, string(item), ref.ArtifactID, ref.Revision, s.config.Clock().UTC())
		if err != nil {
			return step, err
		}
		raw, err = artifact.ReadPayload(ctx, grant, *ref)
		if err != nil {
			return step, err
		}
	} else {
		raw, err = s.readChecklistReviewAttachment(ctx, scope, item, attempt.AttemptID, *step.PayloadAttachment)
		if err != nil {
			return step, err
		}
	}
	return tracker.DecodeNativeStepPayload(raw, step)
}

func (s *Service) readChecklistReviewAttachment(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, attempt string, ref tracker.NativeStepAttachment) ([]byte, error) {
	payload := attachment.RetainedReference{ID: ref.ID, SHA256: ref.SHA256, Size: ref.Size}
	if ref.OrganizationID != scope.Organization || ref.ProjectID != scope.Project || ref.WorkItemID != item || payload.Validate(string(scope.Organization)) != nil {
		return nil, artifact.ErrIntegrity
	}
	var record attachmentstore.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Attachment(ctx, scope.Project, ref.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	key, err := attachment.Key(string(scope.Organization), ref.ID)
	if err != nil {
		return nil, err
	}
	if record.OrganizationID != scope.Organization || record.Metadata.ProjectID != string(scope.Project) || record.Metadata.WorkItemID != string(item) ||
		record.EvidenceAttemptID != attempt || record.Metadata.SHA256 != ref.SHA256 || record.Metadata.Size != ref.Size ||
		record.Metadata.DeletedAt != nil || record.ObjectDeletedAt != nil || record.PendingContent != nil ||
		record.CustomerOwned || record.StorageBindingID != "" || record.ObjectKey != "" && record.ObjectKey != key {
		return nil, artifact.ErrIntegrity
	}
	content := s.storageArchiveContent()
	if content == nil {
		return nil, artifact.ErrStorage
	}
	raw, err := content(ctx, entryauthority.ContentRequest{Organization: string(scope.Organization), Attachment: &payload})
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.Size || artifact.Digest(raw) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	return raw, nil
}
