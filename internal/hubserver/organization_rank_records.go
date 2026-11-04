package hubserver

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) organizationRankRecord(ctx context.Context, r tenantstore.Reader, scope nativeScope) (organizationProjectRank, []tracker.ProjectID, error) {
	value := organizationProjectRank{ProjectIDs: []tracker.ProjectID{}}
	if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
		return value, nil, err
	}
	owner, err := organizationVisibleReader(r)
	if err != nil {
		return value, nil, err
	}
	value.Revision, err = owner.OrganizationSchedulingRevision(ctx)
	if err != nil {
		return value, nil, err
	}
	projects, err := owner.OrganizationRankProjects(ctx, 201)
	if err != nil {
		return value, nil, err
	}
	if len(projects) > 200 {
		return value, nil, corestore.ErrInvalidPage
	}
	pageReader, ok := r.(accessstore.ProjectAccessPageReader)
	if !ok {
		return value, nil, ErrProjectParticipants
	}
	grants, err := pageReader.ProjectAccessPage(ctx, projectAccessFilter(scope.credential, "", "", 200))
	if err != nil {
		return value, nil, err
	}
	allowed := make(map[tracker.ProjectID]bool, len(grants))
	for _, grant := range grants {
		allowed[grant.ProjectID] = true
	}
	all := make([]tracker.ProjectID, 0, len(projects))
	for _, project := range projects {
		all = append(all, project.ID)
		if allowed[project.ID] {
			value.ProjectIDs = append(value.ProjectIDs, project.ID)
		}
	}
	return value, all, nil
}
func (s *Service) readOrganizationRankRecord(ctx context.Context, scope nativeScope) (organizationProjectRank, error) {
	var value organizationProjectRank
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		value, _, err = s.organizationRankRecord(ctx, r, scope)
		return err
	})
	return value, projectRecordError(err)
}
func (s *Service) updateOrganizationRankRecord(ctx context.Context, scope nativeScope, change projectRankChange) (any, error) {
	scope.requireHostedAdmin = true
	if len(change.ProjectIDs) > 200 || change.ExpectedRevision < 0 {
		return nil, nativePublicInvalid("Order must include every readable project once")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	var value organizationProjectRank
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		current, all, err := s.organizationRankRecord(ctx, w, scope)
		if err != nil {
			return err
		}
		if current.Revision != change.ExpectedRevision {
			return nativeConflict(tracker.Revision(current.Revision))
		}
		allowed := make(map[tracker.ProjectID]bool, len(current.ProjectIDs))
		for _, id := range current.ProjectIDs {
			allowed[id] = true
		}
		if len(change.ProjectIDs) != len(allowed) {
			return nativePublicInvalid("Order must include every readable project once")
		}
		seen := make(map[tracker.ProjectID]bool, len(change.ProjectIDs))
		for _, id := range change.ProjectIDs {
			if !allowed[id] || seen[id] {
				return nativePublicInvalid("Order must include every readable project once")
			}
			seen[id] = true
		}
		next := 0
		for i, id := range all {
			if allowed[id] {
				all[i] = change.ProjectIDs[next]
				next++
			}
		}
		owner, err := organizationVisibleWriter(w)
		if err != nil {
			return err
		}
		if err := owner.SetOrganizationProjectRanks(ctx, current.Revision, all); err != nil {
			return err
		}
		if scope.credential.Hosted != nil {
			audit := projectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status = "action", "PUT /api/v2/organizations/"+string(scope.organization)+"/project-rank", http.StatusOK
			audit.RecordedAt = s.config.now()
			audit.RecordedAtNS = audit.RecordedAt.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		value = organizationProjectRank{Revision: current.Revision + 1, ProjectIDs: append([]tracker.ProjectID{}, change.ProjectIDs...)}
		return nil
	})
	return value, projectRecordError(err)
}
