package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
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
	credential := scope.credential
	bind := func(project tracker.ProjectID, item string) nativeScope {
		bound := scope
		bound.project, bound.actionItem = project, item
		return bound
	}
	caller := threads.Caller{
		Organization:   scope.organization,
		Project:        scope.project,
		Item:           scope.actionItem,
		Principal:      operatorIdentity(credential, string(scope.organization)).PrincipalID,
		ActorPrincipal: scope.actor().PrincipalID,
		CredentialID:   credential.ID,
		Name:           credential.Name,
		Agent:          credential.Agent,
		HostedRole:     credential.HostedRole,
		Hosted:         credential.Hosted != nil,
		EntryKey:       credential.EntryKey != nil,
		Personal:       credential.HostedKeyKind == "personal" || credential.EntryKey != nil && credential.EntryKey.Kind == "personal",
		AllProjects:    credential.Hosted == nil && credential.Scope == apiScopeAdmin && !credential.NativeOnly,
		ProjectFilter:  projectAccessFilter(credential, "", "", 0),
		Authority: func(project tracker.ProjectID, item string) accessstore.Authority {
			return s.accessAuthority(bind(project, item), false)
		},
		Authorize: func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, item string, write bool) error {
			return s.authorizeAccessScope(ctx, reader, bind(project, item), write)
		},
		RequireItem: func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item string, id string) error {
			return requireDashboardRecordItem(ctx, reader, bind(project, item), id)
		},
		ReadAttachment: func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, item string, id string) error {
			var metadata attachment.Metadata
			return s.readAttachmentRecordWith(ctx, reader, bind(project, item), id, &metadata)
		},
	}
	if credential.Hosted != nil {
		caller.HostedName = strings.TrimSpace(credential.Hosted.FirstName + " " + credential.Hosted.LastName)
		caller.HostedSubject = credential.Hosted.Subject
	}
	return caller
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
