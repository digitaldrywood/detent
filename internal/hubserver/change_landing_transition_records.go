package hubserver

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func landingTransitionReference(item corestore.WorkItem, change eventstore.Change, version eventstore.Version, attempt leasestore.Attempt, event eventstore.Event) *tracker.NativeChangeReference {
	if attempt.OrganizationID != item.OrganizationID || attempt.IssueID != item.ID || attempt.WorkItemRevision != item.Revision || attempt.DispatchGeneration != item.DispatchGeneration || event.OrganizationID != item.OrganizationID || event.ProjectID != item.ProjectID || event.WorkItemID != item.NativeID || event.AttemptID != attempt.ID || change.OrganizationID != item.OrganizationID || change.ProjectID != item.ProjectID || change.WorkItemID != item.NativeID || change.CurrentVersionID == nil || *change.CurrentVersionID != version.ID || version.OrganizationID != item.OrganizationID || version.ChangeID != change.ID || version.ID == "" || version.HeadSHA == "" || event.LandingChangeID != change.ID || event.LandingVersionID != version.ID || event.LandingHeadSHA != version.HeadSHA {
		return nil
	}
	return &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
}

func (s *Service) landingTransitionSource(ctx context.Context, scope nativeScope, item corestore.WorkItem) (*tracker.NativeChangeReference, error) {
	var reference *tracker.NativeChangeReference
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.LatestAttempt(ctx, item.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		event, err := r.LatestRunEvent(ctx, scope.project, attempt.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil || event.LandingVersionID == "" {
			return err
		}
		change, err := r.LatestChange(ctx, scope.project, item.NativeID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil || change.CurrentVersionID == nil {
			return err
		}
		version, err := r.Version(ctx, change.ID, *change.CurrentVersionID)
		if err != nil {
			return err
		}
		reference = landingTransitionReference(item, change, version, attempt, event)
		return nil
	})
	return reference, err
}
