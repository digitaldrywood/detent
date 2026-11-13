package orgsettings

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) rankRecord(ctx context.Context, r tenantstore.Reader, scope hubapi.Scope) (projectsettings.Rank, []tracker.ProjectID, error) {
	value := projectsettings.Rank{ProjectIDs: []tracker.ProjectID{}}
	if err := scope.Authorize(ctx, r, false); err != nil {
		return value, nil, err
	}
	owner, err := s.VisibleReader(r)
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
		return value, nil, s.deps.Unavailable
	}
	grants, err := pageReader.ProjectAccessPage(ctx, s.deps.Access)
	if err != nil {
		return value, nil, err
	}
	grants, err = s.deps.Granted(ctx, r, grants)
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

func (s *Service) ReadRank(ctx context.Context, scope hubapi.Scope) (projectsettings.Rank, error) {
	var value projectsettings.Rank
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		value, _, err = s.rankRecord(ctx, r, scope)
		return err
	})
	return value, s.deps.RecordError(err)
}

func (s *Service) UpdateRank(ctx context.Context, scope hubapi.Scope, change projectsettings.RankChange) (any, error) {
	if len(change.ProjectIDs) > 200 || change.ExpectedRevision < 0 {
		return nil, hubapi.PublicInvalid("Order must include every readable project once")
	}
	if err := s.deps.Recheck(ctx); err != nil {
		return nil, err
	}
	var value projectsettings.Rank
	err := s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := scope.Authorize(ctx, w, true); err != nil {
			return err
		}
		current, all, err := s.rankRecord(ctx, w, scope)
		if err != nil {
			return err
		}
		if current.Revision != change.ExpectedRevision {
			return hubapi.Conflict(tracker.Revision(current.Revision))
		}
		allowed := make(map[tracker.ProjectID]bool, len(current.ProjectIDs))
		for _, id := range current.ProjectIDs {
			allowed[id] = true
		}
		if len(change.ProjectIDs) != len(allowed) {
			return hubapi.PublicInvalid("Order must include every readable project once")
		}
		seen := make(map[tracker.ProjectID]bool, len(change.ProjectIDs))
		for _, id := range change.ProjectIDs {
			if !allowed[id] || seen[id] {
				return hubapi.PublicInvalid("Order must include every readable project once")
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
		owner, err := s.VisibleWriter(w)
		if err != nil {
			return err
		}
		if err := owner.SetOrganizationProjectRanks(ctx, current.Revision, all); err != nil {
			return err
		}
		if s.deps.Audit != nil {
			audit := *s.deps.Audit
			audit.Event, audit.Route, audit.Status = "action", "PUT /api/v2/organizations/"+string(scope.Organization)+"/project-rank", http.StatusOK
			audit.RecordedAt = s.deps.Now()
			audit.RecordedAtNS = audit.RecordedAt.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		value = projectsettings.Rank{Revision: current.Revision + 1, ProjectIDs: append([]tracker.ProjectID{}, change.ProjectIDs...)}
		return nil
	})
	return value, s.deps.RecordError(err)
}
