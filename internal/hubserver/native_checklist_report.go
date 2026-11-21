package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readExecutionChecklistReport(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, attempt *tracker.NativeAttempt) (*tracker.ExecutionChecklist, error) {
	list, err := s.readExecutionChecklist(ctx, scope, attempt.AttemptID)
	if err != nil || list == nil {
		return list, err
	}
	s.reportNoChangeChecklistGate(ctx, scope, item, attempt, list)
	var acceptance, statusChecks bool
	for i := range list.Checks {
		check := &list.Checks[i]
		switch check.Kind {
		case tracker.CheckAcceptance:
			acceptance = true
			if check.Status == tracker.CheckManual {
				check.Status, check.Head, check.Tree, check.Receipt, check.CheckedAt = tracker.CheckNeedsReview, "", "", "", time.Time{}
			}
		case tracker.CheckStatusGate:
			statusChecks = true
		}
	}
	if !acceptance && !statusChecks {
		return list, nil
	}
	var version eventstore.Version
	var approval eventstore.Evidence
	var issueRow corestore.WorkItem
	var issueVersion eventstore.CollaborationVersion
	var committedLanding eventstore.LandingReceipt
	var landedAt time.Time
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return list, nil
	}
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		change, err := r.LatestChange(ctx, scope.Project, item)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if change.WorkItemID != item || change.CurrentVersionID == nil {
			return nil
		}
		current, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
		if err != nil {
			return err
		}
		if current.AttemptID != attempt.AttemptID || current.PolicyRevisionID != list.PolicyID {
			return nil
		}
		version = current
		if reader, ok := r.(eventstore.ChecklistApprovalReader); ok && acceptance {
			approval, err = reader.CurrentChecklistApproval(ctx, change, version.ID)
			if err != nil {
				return err
			}
		}
		if approval.Sequence > 0 {
			issueRow, err = r.WorkItem(ctx, scope.Project, item)
			if err != nil {
				return err
			}
			issueVersion, err = r.CollaborationVersion(ctx, string(item), issueRow.Revision)
			if err != nil {
				return err
			}
		}
		if statusChecks && change.LandedVersionID != nil && *change.LandedVersionID == version.ID {
			landed, err := eventstore.VersionLandingFromChange(change)
			if err != nil {
				return err
			}
			receipt, err := r.LatestLandingReceipt(ctx, scope.Project, item)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if receipt.VersionID == landed.VersionID && receipt.MergeSHA == landed.MergeSHA && receipt.BaseRef == landed.BaseRef {
				committedLanding, landedAt = receipt, landed.At
			}
		}
		return nil
	})
	if err != nil || version.ID == "" {
		return list, err
	}

	ctx = s.runDetailContext(ctx)
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	var review tracker.ChangeReview
	if approval.Sequence > 0 && !approval.CreatedAt.Before(list.FrozenAt) {
		issue, err := s.hydrateCanonicalWorkItemRecord(ctx, issueRow, issueVersion)
		if err != nil {
			return nil, err
		}
		if list.Revision == progresscheck.ChecklistRevision(issue.Body, list.PolicyID) {
			raw, err := content.Evidence(ctx, approval)
			if err != nil {
				return nil, err
			}
			review, err = eventstore.DecodeChangeReview(approval, raw)
			if err != nil {
				return nil, err
			}
		}
	}
	var landing *tracker.NativeLandingReceipt
	if committedLanding.VersionID == version.ID {
		receipt, err := content.LandingReceipt(ctx, committedLanding)
		if err != nil {
			return nil, err
		}
		if receipt.Landed && receipt.ChangeID == version.ChangeID && receipt.HeadSHA == version.HeadSHA && receipt.CI != nil {
			receipt.ObservedAt = landedAt
			landing = &receipt
		}
	}
	if statusChecks && landing == nil {
		ref := tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
		landing, _, err = s.readNativeLandingObservation(ctx, scope, item, ref)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return nil, err
		}
	}
	for i := range list.Checks {
		check := &list.Checks[i]
		switch check.Kind {
		case tracker.CheckAcceptance:
			if review.ID != "" && (check.Status == tracker.CheckNeedsReview || check.Head != version.HeadSHA) {
				check.Status, check.Head, check.Tree, check.Receipt, check.CheckedAt = tracker.CheckManual, version.HeadSHA, "", review.ID, review.CreatedAt
			}
		case tracker.CheckStatusGate:
			if landing == nil {
				continue
			}
			if status, id := progresscheck.ChecklistStatusGate(*check, version.HeadSHA, landing.CI); status != "" {
				if check.Head != version.HeadSHA {
					check.Tree = ""
				}
				check.Status, check.Head, check.Receipt, check.CheckedAt = status, version.HeadSHA, id, landing.ObservedAt
			}
		}
	}
	return list, nil
}
