package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ChangeVersionRows(ctx context.Context, project tracker.ProjectID, requests []tenantstore.ChangeVersionRequest) ([]tenantstore.ChangeVersionRows, error) {
	if project == "" || len(requests) > 100 {
		return nil, corestore.ErrInvalidPage
	}
	result := make([]tenantstore.ChangeVersionRows, len(requests))
	items, changes, versions, heads := []string{}, []string{}, []string{}, []string{}
	for i, request := range requests {
		if request.WorkItemID == "" || request.Reference.ChangeID == "" || request.Reference.VersionID == "" || request.Reference.HeadSHA == "" {
			return nil, corestore.ErrInvalidPage
		}
		result[i] = tenantstore.ChangeVersionRows{OrganizationID: s.organization, ProjectID: project, Request: request}
		items, changes = append(items, string(request.WorkItemID)), append(changes, request.Reference.ChangeID)
		versions, heads = append(versions, request.Reference.VersionID), append(heads, request.Reference.HeadSHA)
	}
	if len(requests) == 0 {
		return result, nil
	}
	publications, err := s.queries.ClaimVersionPublications(ctx, pgquery.ClaimVersionPublicationsParams{OrganizationID: s.organization, ProjectID: project, VersionIds: versions})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for _, row := range publications {
		if row.RequestIndex < 1 || row.RequestIndex > int64(len(result)) {
			return nil, corestore.ErrInvalidPage
		}
		p := row.NativePrPublication
		result[row.RequestIndex-1].Publication = &eventstore.NativePRPublication{OrganizationID: p.OrganizationID, VersionID: p.VersionID, ChangeID: p.ChangeID, Repository: p.Repository, BaseRef: p.BaseRef, Branch: p.Branch, HeadSHA: p.HeadSHA, PolicyID: p.PolicyID, SourceAttemptID: stringValue(p.SourceAttemptID), RecorderAttemptID: p.RecorderAttemptID, External: tracker.ChangeExternalReference{Provider: p.Provider, ID: p.ExternalID, URL: p.ExternalURL}}
	}
	landings, err := s.queries.ClaimVersionLandingObservations(ctx, pgquery.ClaimVersionLandingObservationsParams{OrganizationID: s.organization, ProjectID: project, WorkItemIds: items, ChangeIds: changes, VersionIds: versions, HeadShas: heads})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for _, row := range landings {
		if row.RequestIndex < 1 || row.RequestIndex > int64(len(result)) {
			return nil, corestore.ErrInvalidPage
		}
		result[row.RequestIndex-1].Landing = &eventstore.LandingObservation{Event: eventRecord(row.CollaborationEvent), FromState: row.LandingFromState, TargetState: row.LandingTargetState}
	}
	return result, nil
}
