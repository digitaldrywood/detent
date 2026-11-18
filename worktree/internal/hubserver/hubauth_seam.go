package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requestAuth() *hubauth.Authenticator {
	deps := hubauth.Deps{
		TrustedProxy: s.config.TrustedProxy, GenerateToken: s.config.deps.GenerateToken, Repository: s.config.TenantRepository, AccessRepository: s.config.AccessRepository, Organization: s.accessOrganization(), Now: s.config.Clock,
		Classify: hubscope.OperatorAuthorizationError, SharedSession: s.sharedHostedSession, CompleteInvitation: s.completePendingHostedInvitation,

		BoardCredential: func(ctx context.Context) (hubauth.Credential, bool) {
			credential, ok := entryBoardCredential(ctx)
			return hubscope.AuthCredential(credential), ok
		},
		SharedClaims: hostedSharedClaims,

		RunnerEventRoute: runnerEventCommandRoute,
		RunnerEventContext: func(ctx context.Context) context.Context {
			return context.WithValue(ctx, runnerEventAuthorityKey{}, true)
		},
		StoreCredential: func(c echo.Context, credential hubauth.Credential) {
			c.Set("hub_api_credential", hubscope.NativeCredential(credential))
			c.Set("hub_auth_credential", credential)
		},
	}
	if s.config.Hosted != nil {
		deps.Hosted = &hubauth.Hosted{OrganizationID: s.config.Hosted.OrganizationID, Shared: s.config.Hosted.SharedEntry != nil, StaffEmails: s.config.Hosted.StaffEmails, SupportActors: s.config.Hosted.SupportActors}
	}
	if s.config.Hosted != nil && s.config.Hosted.Provider != nil {
		deps.Memberships = s.config.Hosted.Provider.Memberships
		deps.CurrentSession = s.config.Hosted.Provider.CurrentSession
	}
	if s.hostedSessions != nil {
		deps.SessionLookup = s.hostedSessions.Authenticate
	}
	return hubauth.New(deps)
}
func (s *Service) authInput(scope hubscope.NativeScope) hubauth.Input {
	input := hubauth.Input{Organization: scope.Organization, Project: scope.Project, Credential: hubscope.AuthCredential(scope.Credential), Admin: scope.RequireHostedAdmin, Action: scope.Action, IntegrationRepository: scope.IntegrationRepository, IntegrationOwned: scope.IntegrationOwned}
	if scope.GitHubIntakeRevision != nil {
		input.IntegrationFence = func(ctx context.Context, r accessstore.Reader) error {
			if fence := scope.GitHubIntakeRevision; fence != nil {
				if fence.BatchID != "" {
					owner, ok := r.(projectstore.IntakeBatchReader)
					if !ok {
						return accessstore.ErrDenied
					}
					batch, err := owner.ProjectIntakeBatch(ctx, scope.Project)
					if err != nil {
						return err
					}
					if batch.ID != fence.BatchID || batch.Revision != fence.Revision || batch.Status != "discovering" && batch.Status != "importing" {
						return corestore.ErrRevisionConflict
					}
					if fence.Authority == nil {
						return operatortool.ErrAccessDenied
					}
					reader, ok := r.(tenantstore.Reader)
					if !ok {
						return hubapi.ErrPostgresFeatureUnavailable
					}
					if err := s.gitHubBatchRequestAuthority(ctx, reader, *fence.Authority, fence.RequireAdminKey); err != nil {
						return err
					}
					if fence.Reporter != nil {
						if err := s.requireGitHubBatchRunner(ctx, reader, *fence.Reporter, fence.Reporter.Credential.Runner.RunnerID, scope.IntegrationRepository, s.config.Clock()); err != nil {
							return err
						}
					}

				}
				if fence.ImportID != "" {
					owner, ok := r.(projectstore.GitHubImportReader)
					if !ok {
						return accessstore.ErrDenied
					}
					job, err := owner.GitHubImport(ctx, scope.Project, fence.ImportID)
					if err != nil {
						return err
					}
					if job.Revision != fence.Revision {
						return corestore.ErrRevisionConflict
					}
				}
			}
			return nil
		}
	}
	return input
}

func (s *Service) operatorAuthOptions() hubauth.OperatorOptions {
	options := hubauth.OperatorOptions{
		Classify: hubscope.OperatorAuthorizationError,
		Error:    func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Refuse: func(c echo.Context, status int, code, message string, body any) error {
			return s.refusedAPIError(c, status, code, message, body)
		},
		AccountCredential: func(ctx context.Context, hash string) (hubauth.Credential, error) {
			c, err := s.hostedAccountCredential(ctx, hash)
			return hubscope.AuthCredential(c), err
		},
		EntryOrigin: func(ctx context.Context, origin string) (hubauth.Credential, error) {
			c, err := s.currentEntryOriginCredential(ctx, origin)
			return hubscope.AuthCredential(c), err
		},
		CurrentSession: s.WebSession, AccessTime: operatorAccessTime,
		Authority: func(ctx context.Context, c hubauth.Credential, org string) (operatortool.Authority, error) {
			return s.operatorCurrentAuthority(ctx, hubscope.NativeCredential(c), org)
		},
		Bind: func(ctx context.Context, organization, origin string, c hubauth.Credential, resolve hubauth.Resolver) context.Context {
			nativeResolve := func(ctx context.Context) (hubapi.APICredential, error) {
				c, err := resolve(ctx)
				return hubscope.NativeCredential(c), err
			}
			ctx = context.WithValue(ctx, operatorCredentialKey{}, billingAuthorization(nativeResolve))
			if c.EntryKey != nil {
				ctx = context.WithValue(ctx, hubscope.OperatorEntryOriginKey{}, origin)
			}
			ctx = context.WithValue(ctx, hubOperatorResolverKey{}, nativeResolve)
			return context.WithValue(ctx, hubscope.NativeOperatorScopeKey{}, func(ctx context.Context) (hubscope.NativeScope, error) {
				c, err := resolve(ctx)
				return hubscope.NativeScope{Organization: tracker.OrganizationID(organization), Credential: hubscope.NativeCredential(c)}, err
			})
		},
		Catalog: func(ctx context.Context, c hubauth.Credential, org string) context.Context {
			return s.withOperatorCatalog(ctx, hubscope.NativeCredential(c), org)
		},
		DashboardURL: s.operatorDashboardURL(),
	}
	if s.config.Hosted != nil {
		options.PublicURL = s.config.Hosted.PublicURL
		options.ResolveEntryKey = s.config.Hosted.SharedEntry != nil && s.config.Hosted.SharedEntry.ResolveKey != nil
	}
	return options
}
func (s *Service) operatorCheckOptions() hubauth.AuthorityOptions {
	return hubauth.AuthorityOptions{
		Classify: hubscope.OperatorAuthorizationError,
		Base: func(input hubauth.Input) operatortool.Authority {
			scope := hubscope.ScopeFromAuthInput(input)
			credential := scope.Credential
			return operatortool.Authority{Explainer: operatorWorkReads{service: s, scope: scope}, WorkReads: operatorWorkReads{service: s, scope: scope}, Changes: hubChangeApplication{service: s, scope: scope}, BindContext: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, operatorScopeKey{}, scope)
				ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(context.Context) (hubapi.APICredential, error) { return credential, nil })
				ctx = context.WithValue(ctx, hubscope.NativeOperatorScopeKey{}, func(context.Context) (hubscope.NativeScope, error) { return scope, nil })
				return context.WithValue(ctx, operatorCredentialKey{}, billingAuthorization(func(context.Context) (hubapi.APICredential, error) { return credential, nil }))
			}}
		},
		KeyCredential: func(ctx context.Context, c hubauth.Credential) (hubauth.Credential, error) {
			v, err := s.hostedKeyCredentialFor(ctx, hubscope.NativeCredential(c))
			return hubscope.AuthCredential(v), err
		},
		RunnerAdministration: func(ctx context.Context, c hubauth.Credential) error {
			return s.requireHostedRunnerAdministration(ctx, hubscope.NativeCredential(c))
		},
		RecheckMutation: func(ctx context.Context, input hubauth.Input) error {
			return s.recheckProjectRecordMutation(ctx, hubscope.ScopeFromAuthInput(input))
		},
		Resource: func(ctx context.Context, input hubauth.Input, r operatortool.Requirement) error {
			return s.checkOperatorResource(ctx, nil, hubscope.ScopeFromAuthInput(input), r)
		},
	}
}

func (s *Service) resolveHubauthNativeScope(ctx context.Context, c echo.Context) error {
	a := s.requestAuth()
	err := a.ResolveNative(ctx, c, hubauth.NativeOptions{
		Credential: func(c echo.Context) (hubauth.Credential, bool) {
			v, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			return hubscope.AuthCredential(v), ok
		},
		EventScope: func(c echo.Context, input hubauth.Input) bool {
			return s.runnerEventCommandScope(c, hubscope.ScopeFromAuthInput(input))
		},
		ArtifactRead: artifactReadGrantRequest, RelayTicket: relayTicketRequest, LeaseRoute: runnerLeaseMutationRoute, RepositoryRoute: runnerRepositoryReportRoute,
		AuthorizeLease: func(ctx context.Context, input hubauth.Input) error {
			return s.authorizeRunnerLeaseProject(ctx, hubscope.ScopeFromAuthInput(input))
		},
		AuthorizeRepository: func(ctx context.Context, input hubauth.Input) error {
			return s.authorizeRunnerRepositoryProject(ctx, hubscope.ScopeFromAuthInput(input))
		},
		ReadProject: func(ctx context.Context, input hubauth.Input, write, models bool) error {
			record, err := s.readAuthorizedProjectRecord(ctx, hubscope.ScopeFromAuthInput(input), write, models)
			if err == nil {
				c.Set("native_project_record", record)
			}
			return err
		},
		ReadItem: func(ctx context.Context, input hubauth.Input, item string) error {
			_, _, err := s.readWorkItemRecord(ctx, hubscope.ScopeFromAuthInput(input), item)
			return err
		},
		RecordError: hubapi.ProjectRecordError,
		Store: func(c echo.Context, input hubauth.Input) {
			c.Set("native_scope", hubscope.ScopeFromAuthInput(input))
			c.Set("hub_request_scope", a.Scope(input))
		},
	})
	return hubapi.FromFeatureError(err)
}
func (s *Service) requireHubauthAPI(allowed ...hubapi.APIScope) echo.MiddlewareFunc {
	roles := make([]hubauth.TokenScope, len(allowed))
	for i, role := range allowed {
		roles[i] = hubauth.TokenScope(role)
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			return s.requestAuth().RequireAPI(s.hubauthAPIOptions(), roles...)(next)(c)
		}
	}
}

func (s *Service) hubauthAPIOptions() hubauth.APIOptions {
	return hubauth.APIOptions{
		Classify: hubscope.OperatorAuthorizationError,
		Error:    func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Refuse: func(c echo.Context, status int, code, message string, body any) error {
			return s.refusedAPIError(c, status, code, message, body)
		},
		IsNotFound:           func(err error) bool { return hubapi.IsNativeNotFound(hubapi.FromFeatureError(err)) },
		HostedAdministration: s.requireHostedAdministration,
		ArtifactPublisher: func(c echo.Context, v hubauth.Credential) bool {
			return s.hostedArtifactPublisher(c, hubscope.NativeCredential(v))
		},
		ChangePrincipal: func(c echo.Context, v hubauth.Credential) bool {
			return s.hostedChangeCheckPrincipal(c, hubscope.NativeCredential(v))
		},
		ConfigurationRank: configurationRankRequest, CompatibilityResource: s.requireCompatibilityResource,
		AdmitRunner: func(c echo.Context, v hubauth.Credential) error {
			return s.admitRunnerRequest(c, hubscope.NativeCredential(v))
		},
	}
}

func (s *Service) requireHubauthNative(allowed ...hubapi.APIScope) echo.MiddlewareFunc {
	roles := make([]hubauth.TokenScope, len(allowed))
	for i, role := range allowed {
		roles[i] = hubauth.TokenScope(role)
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			return s.requestAuth().Native(hubauth.NativeMiddlewareOptions{API: s.hubauthAPIOptions(), Authorize: s.authorizeNativeReadRequest,
				ArchiveContext: func(c echo.Context, _ hubapi.Scope) context.Context {
					return s.archiveRequestContext(c, hubscope.NativeRequestScope(c))
				}, Audit: s.hostedAudit,
			}, roles...)(next)(c)
		}
	}
}
