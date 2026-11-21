package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readExecutionChecklistReport(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, attempt string) (*tracker.ExecutionChecklist, error) {
	list, err := s.readExecutionChecklist(ctx, scope, attempt)
	if err != nil || list == nil {
		return list, err
	}
	acceptance := slices.ContainsFunc(list.Checks, func(check tracker.ExecutionCheck) bool { return check.Kind == tracker.CheckAcceptance })
	statusChecks := slices.ContainsFunc(list.Checks, func(check tracker.ExecutionCheck) bool { return check.Kind == tracker.CheckStatusGate })
	if !acceptance && !statusChecks {
		return list, nil
	}
	var version eventstore.Version
	var approval eventstore.Evidence
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
		if current.AttemptID != attempt || current.PolicyRevisionID != list.PolicyID {
			return nil
		}
		version = current
		if reader, ok := r.(eventstore.CurrentChangeEvidenceReader); ok && acceptance {
			evidence, err := reader.CurrentChangeEvidence(ctx, change.ID, version.ID, change.EvidenceSequence, nil)
			if err != nil {
				return err
			}
			for _, row := range evidence {
				if row.ReviewKind != "human" || row.ActorKind != "human" {
					continue
				}
				if row.Decision == "changes_requested" {
					approval = eventstore.Evidence{}
					break
				}
				if row.Decision == "approved" {
					approval = row
				}
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
	if approval.Sequence > 0 {
		raw, err := content.Evidence(ctx, approval)
		if err != nil {
			return nil, err
		}
		review, err = eventstore.DecodeChangeReview(approval, raw)
		if err != nil {
			return nil, err
		}
	}
	var landing *tracker.NativeLandingReceipt
	if statusChecks {
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
			if review.ID != "" && (check.Status == tracker.CheckNeedsReview || check.Status == tracker.CheckManual || check.Head != version.HeadSHA) {
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
