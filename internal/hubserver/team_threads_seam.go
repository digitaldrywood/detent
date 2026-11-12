package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type teamUnreadSummary = threads.UnreadSummary

func threadInvalid() error {
	return fromFeatureError(threads.Invalid())
}

func threadRecordError(err error) error {
	return fromFeatureError(threads.RecordError(err, projectRecordError))
}

func (s *Service) threadsService() *threads.Service {
	return threads.New(threads.Deps{
		Repository:   s.config.TenantRepository,
		Details:      s.runDetailStore,
		Now:          s.config.now,
		Organization: func() tracker.OrganizationID { return tracker.OrganizationID(s.appOrganizationID()) },
		RecordError:  projectRecordError,
		IsNotFound:   isNativeNotFound,
	})
}

func (s *Service) threadCaller(scope nativeScope) threads.Caller {
	bind := func(project tracker.ProjectID, item string) nativeScope {
		bound := scope
		bound.project, bound.actionItem = project, item
		return bound
	}
	actor := scope.actor()
	return threads.NewCaller(s.requestAuth(), s.authInput(scope), threads.CallerDeps{Item: scope.actionItem, Actor: &actor,
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

func (s *Service) teamThreadRecordOperation(ctx context.Context, scope nativeScope, name string, in teamThreadInput) (any, error) {
	value, err := s.threadsService().Operation(ctx, s.threadCaller(scope), name, in)
	return value, fromFeatureError(err)
}

func (s *Service) listThreadRecords(ctx context.Context, scope nativeScope, in teamThreadInput) (teamThreadPage, error) {
	page, err := s.threadsService().List(ctx, s.threadCaller(scope), in)
	return page, fromFeatureError(err)
}

func (s *Service) requestTeamUnread(ctx context.Context, scope nativeScope, projects []string, refresh bool) (teamUnreadSummary, error) {
	summary, err := s.threadsService().RequestUnread(ctx, s.threadCaller(scope), projects, refresh)
	return summary, fromFeatureError(err)
}

func (s *Service) maintainTeamUnread(ctx context.Context) error {
	return s.threadsService().MaintainUnread(ctx)
}

func (s *Service) advanceTeamUnread(ctx context.Context) (bool, int, error) {
	return s.threadsService().AdvanceUnread(ctx)
}

func (s *Service) advanceTeamUnreadRecord(ctx context.Context) (bool, int, error) {
	return s.threadsService().AdvanceUnreadRecord(ctx)
}

func (s *Service) threadHTTPDeps() threads.HTTPDeps {
	return threads.HTTPDeps{Service: s.threadsService, Auth: s.requestAuth, Operator: s.operatorAuthority,
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
		Resolve: func(ctx context.Context) (hubauth.Input, error) {
			resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
			if !ok {
				return hubauth.Input{}, operatortool.ErrAccessDenied
			}
			scope, err := resolve(ctx)
			return s.authInput(scope), err
		},
		Caller: func(input hubauth.Input) threads.Caller { return s.threadCaller(nativeAuthInput(input)) },
		NotifyUnread: func() {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		},
	}
}
func (s *Service) resolveThreadAuthority(ctx context.Context, name string, required apikey.Scope) (context.Context, nativeScope, error) {
	ctx, input, err := threads.NewHTTP(s.threadHTTPDeps()).Authority(ctx, name, required)
	return ctx, nativeAuthInput(input), fromFeatureError(err)
}
