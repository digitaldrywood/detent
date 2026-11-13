package conversations

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type Config struct {
	TenantRepository            tenantstore.Repository
	Now                         func() time.Time
	ConversationDispatch        tenantstore.DispatchRequests
	ConversationWorkerAuthority ConversationWorkerAuthority
}

type LinkedWorkItem struct {
	Existing    bool
	Create      bool
	Issue       tracker.NativeIssue
	ProjectName string
}

type PreferenceIssue interface {
	Lock(context.Context, tenantstore.Writer) error
	Write(context.Context, tenantstore.Writer) error
	Changed() bool
}

type noPreferenceIssue struct{}

func (noPreferenceIssue) Lock(context.Context, tenantstore.Writer) error  { return nil }
func (noPreferenceIssue) Write(context.Context, tenantstore.Writer) error { return nil }
func (noPreferenceIssue) Changed() bool                                   { return false }

type CommandDeliverer interface {
	DeliverCoordinatorCommands(context.Context, string) error
}

type Deps struct {
	Config                         func() Config
	Conversations                  func() *ConversationService
	Logger                         *slog.Logger
	Broker                         *eventbroker.Broker
	Contents                       func(attachment.ArchiveContentStore) attachment.ArchiveContentStore
	UsageSink                      ConversationUsageSink
	NewCoordinator                 func(*ConversationService) ConversationCoordinator
	NewControls                    func(*ConversationService) ConversationControlRouter
	Ready                          func() bool
	Organization                   func() string
	Authorization                  hubscope.Authorization
	BrowserLink                    func(context.Context, string) (string, error)
	ArchiveContext                 func(context.Context, hubauth.Input, bool) context.Context
	WithArchive                    func(context.Context, []byte) ([]byte, error)
	BindCoordinatorConnection      func(context.Context, string) (context.Context, error)
	AttachmentQuota                func(context.Context, tenantstore.Reader) (int64, error)
	LimitError                     func(resource string, allowance, consumption int64) error
	ReadProject                    func(context.Context, hubauth.Input) (tracker.NativeProject, error)
	ReadWorkItem                   func(context.Context, hubauth.Input, string) (tracker.NativeIssue, error)
	ReadNativeRuntime              func(context.Context, hubauth.Input, string) (tracker.NativeRuntimeEvidence, error)
	WorkItemReceipt                func(ctx context.Context, scope hubauth.Input, operation, key, hash string) (bool, error)
	LinkWorkItem                   func(ctx context.Context, scope hubauth.Input, operation string, request tracker.CreateIssue, link *ConversationLinkMutation) error
	PreferenceIssue                func(context.Context, hubauth.Input, ConversationRecord, conversation.Preferences) (PreferenceIssue, error)
	GrantedProjects                func(context.Context, accessstore.Reader, tracker.OrganizationID, hubauth.Credential, []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error)
	ProjectParticipantsUnavailable error
	RequireFeature                 func(ctx context.Context, reader tenantstore.Reader, feature string, now time.Time) error
	ModelChoiceGranted             func(context.Context, tenantstore.Reader, tracker.OrganizationID, time.Time) (bool, error)
	CheckMonthlyLease              func(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, lease string, now time.Time) error
	WorkerAuthorityAt              func(time.Time) ConversationWorkerAuthority
	IssueBrowserPath               func(context.Context, Queryer, string) (string, error)
	ResolveWorkItems               func(context.Context, Queryer, []*ConversationRecord) error
	APIError                       func(echo.Context, error) error
	InvalidRequest                 func(echo.Context, error) error
}

type Server struct {
	deps Deps
}

func NewServer(deps Deps) *Server {
	return &Server{deps: deps}
}

func (s *Server) config() Config {
	return s.deps.Config()
}

func (s *Server) conversations() *ConversationService {
	return s.deps.Conversations()
}

func (s *Server) appOrganizationID() string {
	return s.deps.Organization()
}

func (s *Server) authorizeAccessScope(ctx context.Context, reader accessstore.Reader, scope hubauth.Input, write bool) error {
	return s.deps.Authorization.AuthorizeInput(ctx, reader, scope, write)
}

func (s *Server) authorizeWorkItemRecordScope(ctx context.Context, reader accessstore.Reader, scope hubauth.Input) error {
	return s.deps.Authorization.AuthorizeWorkItemInput(ctx, reader, scope)
}

func (s *Server) canonicalBrowserLink(ctx context.Context, link string) (string, error) {
	return s.deps.BrowserLink(ctx, link)
}

func (s *Server) archiveContext(ctx context.Context, scope hubauth.Input, presign bool) context.Context {
	return s.deps.ArchiveContext(ctx, scope, presign)
}

func (s *Server) bindCoordinatorConnection(ctx context.Context, key string) (context.Context, error) {
	return s.deps.BindCoordinatorConnection(ctx, key)
}

func (s *Server) attachmentQuotaLimit(ctx context.Context, reader tenantstore.Reader) (int64, error) {
	return s.deps.AttachmentQuota(ctx, reader)
}

func (s *Server) readNativeRuntime(ctx context.Context, scope hubauth.Input, item, _ string) (tracker.NativeRuntimeEvidence, error) {
	return s.deps.ReadNativeRuntime(ctx, scope, item)
}

func (s *Server) readWorkItem(ctx context.Context, scope hubauth.Input, id string) (tracker.NativeIssue, error) {
	return s.deps.ReadWorkItem(ctx, scope, id)
}

func (s *Server) grantedAccessProjects(ctx context.Context, reader accessstore.Reader, organization tracker.OrganizationID, credential hubauth.Credential, projects []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error) {
	return s.deps.GrantedProjects(ctx, reader, organization, credential, projects)
}

func (s *Server) Now() time.Time {
	return s.config().Now()
}

func ScopeInput(scope hubscope.NativeScope) hubauth.Input {
	return hubauth.Input{Organization: scope.Organization, Project: scope.Project, Credential: hubscope.AuthCredential(scope.Credential), Admin: scope.RequireHostedAdmin, Action: scope.Action, IntegrationRepository: scope.IntegrationRepository, IntegrationOwned: scope.IntegrationOwned}
}

func staleExecutionError(path, conversationID string, err error) error {
	return ConversationStaleExecutionError(path, conversationID, hubapi.ToFeatureError(err))
}

func (s *Server) apiError(c echo.Context, err error) error {
	return s.deps.APIError(c, err)
}

func (s *Server) invalidRequest(c echo.Context, err error) error {
	return s.deps.InvalidRequest(c, err)
}
