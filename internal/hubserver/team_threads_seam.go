package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type teamUnreadSummary = threads.UnreadSummary

func (s *Service) threadsService() *threads.Service {
	return threads.New(threads.Deps{
		Repository:   s.config.TenantRepository,
		Details:      s.runDetailStore,
		Now:          s.config.Clock,
		Organization: func() tracker.OrganizationID { return tracker.OrganizationID(s.appOrganizationID()) },
		RecordError:  hubapi.ProjectRecordError,
		IsNotFound:   hubapi.IsNativeNotFound,
	})
}

func (s *Service) threadCaller(scope hubscope.NativeScope) threads.Caller {
	bind := func(project tracker.ProjectID, item string) hubscope.NativeScope {
		bound := scope
		bound.Project, bound.ActionItem = project, item
		return bound
	}
	actor := scope.Actor()
	return threads.NewCaller(s.requestAuth(), s.authInput(scope), threads.CallerDeps{Item: scope.ActionItem, Actor: &actor,
		Authorize: func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, item string, write bool) error {
			return s.authorizeAccessScope(ctx, reader, bind(project, item), write)
		},
		RequireItem: func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item, id string) error {
			return requireDashboardRecordItem(ctx, reader, bind(project, item), id)
		},
		ReadAttachment: func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item, id string) error {
			var metadata attachment.Metadata
			return s.readAttachmentRecordWith(ctx, reader, bind(project, item), id, &metadata)
		},
	})
}

func (s *Service) maintainTeamUnread(ctx context.Context) error {
	return s.threadsService().MaintainUnread(ctx)
}

func (s *Service) advanceTeamUnreadRecord(ctx context.Context) (bool, int, error) {
	return s.threadsService().AdvanceUnreadRecord(ctx)
}

func (s *Service) threadHTTPDeps() threads.HTTPDeps {
	return threads.HTTPDeps{Service: s.threadsService, Auth: s.requestAuth, Operator: s.operatorAuthority,
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Resolve: func(ctx context.Context) (hubauth.Input, error) {
			resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
			if !ok {
				return hubauth.Input{}, operatortool.ErrAccessDenied
			}
			scope, err := resolve(ctx)
			return s.authInput(scope), err
		},
		Caller: func(input hubauth.Input) threads.Caller { return s.threadCaller(hubscope.ScopeFromAuthInput(input)) },
		NotifyUnread: func() {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		},
	}
}
