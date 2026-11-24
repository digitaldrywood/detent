package threads

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requestUnread(ctx context.Context, scope access, projects []string, refresh bool) (UnreadSummary, error) {
	request := threadstore.UnreadRequest{Principal: scope.caller.Principal, CredentialID: scope.caller.CredentialID, Refresh: refresh}
	filter := scope.caller.ProjectFilter
	request.UserID, request.MemberPrincipal, request.KeyKind, request.RequireCredentialGrant = filter.UserID, filter.PrincipalID, filter.KeyKind, filter.RequireCredentialGrant
	if scope.caller.AllProjects {
		request.KeyKind = "service"
	}
	if scope.caller.HostedRole == "reporter" {
		request.Reporter = scope.caller.ActorPrincipal
	}
	for _, project := range projects {
		request.Projects = append(request.Projects, tracker.ProjectID(project))
	}
	id, err := hubapi.CommandHash(struct{ Organization, Principal string }{string(scope.caller.Organization), request.Principal})
	if err != nil {
		return UnreadSummary{}, err
	}
	request.ID = id
	hashRequest := request
	hashRequest.Refresh = false
	request.ScopeHash, err = hubapi.CommandHash(hashRequest)
	if err != nil {
		return UnreadSummary{}, err
	}
	var result threadstore.Unread
	err = s.deps.Repository.WriteTenant(ctx, scope.caller.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := scope.authorize(ctx, w, false); err != nil {
			return err
		}
		for _, project := range request.Projects {
			check := scope
			check.project = project
			if err := check.authorize(ctx, w, false); err != nil {
				return err
			}
		}
		var err error
		result, err = w.RequestThreadUnread(ctx, request)
		return err
	})
	return UnreadSummary{Count: result.Count, Pending: result.Pending}, s.recordError(err)
}
func (s *Service) AdvanceUnreadRecord(ctx context.Context) (bool, int, error) {
	return s.AdvanceUnreadPage(ctx, unreadPageSize)
}

func (s *Service) AdvanceUnreadPage(ctx context.Context, limit int) (bool, int, error) {
	limit = min(limit, unreadPageSize)
	organization := s.deps.Organization()
	if organization == "" {
		return false, 0, corestore.ErrTenantMismatch
	}
	var job threadstore.UnreadJob
	var page []threadstore.UnreadRow
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		job, err = r.ThreadUnreadJob(ctx)
		if err != nil {
			return err
		}
		page, err = r.ThreadUnreadPage(ctx, job, limit+1)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	complete := len(page) <= limit
	if !complete {
		page = page[:limit]
	}
	job.OriginalAfter = job.After
	for _, row := range page {
		job.After = row.ID
		if row.Unread {
			job.Partial++
		}
	}
	err = s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error { return w.ApplyThreadUnread(ctx, job, complete) })
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return true, 0, nil
	}
	return true, len(page), err
}
