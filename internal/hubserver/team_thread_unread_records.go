package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requestTeamUnreadRecord(ctx context.Context, scope nativeScope, projects []string, refresh bool) (teamUnreadSummary, error) {
	request := threadstore.UnreadRequest{Principal: operatorIdentity(scope.credential, string(scope.organization)).PrincipalID, CredentialID: scope.credential.ID, Refresh: refresh}
	filter := projectAccessFilter(scope.credential, "", "", 1)
	request.UserID, request.MemberPrincipal, request.KeyKind, request.RequireCredentialGrant = filter.UserID, filter.PrincipalID, filter.KeyKind, filter.RequireCredentialGrant
	if scope.credential.Hosted == nil && scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly {
		request.KeyKind = "service"
	}
	if scope.credential.HostedRole == "reporter" {
		request.Reporter = scope.actor().PrincipalID
	}
	for _, project := range projects {
		request.Projects = append(request.Projects, tracker.ProjectID(project))
	}
	id, err := nativeCommandHash(struct{ Organization, Principal string }{string(scope.organization), request.Principal})
	if err != nil {
		return teamUnreadSummary{}, err
	}
	request.ID = id
	hashRequest := request
	hashRequest.Refresh = false
	request.ScopeHash, err = nativeCommandHash(hashRequest)
	if err != nil {
		return teamUnreadSummary{}, err
	}
	var result threadstore.Unread
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			return err
		}
		for _, project := range request.Projects {
			check := scope
			check.project = project
			if err := s.authorizeAccessScope(ctx, w, check, false); err != nil {
				return err
			}
		}
		var err error
		result, err = w.RequestThreadUnread(ctx, request)
		return err
	})
	return teamUnreadSummary{Count: result.Count, Pending: result.Pending}, threadRecordError(err)
}
func (s *Service) advanceTeamUnreadRecord(ctx context.Context) (bool, int, error) {
	organization := tracker.OrganizationID(s.appOrganizationID())
	if organization == "" {
		return false, 0, corestore.ErrTenantMismatch
	}
	var job threadstore.UnreadJob
	var page []threadstore.UnreadRow
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		job, err = r.ThreadUnreadJob(ctx)
		if err != nil {
			return err
		}
		page, err = r.ThreadUnreadPage(ctx, job, teamUnreadPageSize+1)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	complete := len(page) <= teamUnreadPageSize
	if !complete {
		page = page[:teamUnreadPageSize]
	}
	job.OriginalAfter = job.After
	for _, row := range page {
		job.After = row.ID
		if row.Unread {
			job.Partial++
		}
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error { return w.ApplyThreadUnread(ctx, job, complete) })
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return true, 0, nil
	}
	return true, len(page), err
}
