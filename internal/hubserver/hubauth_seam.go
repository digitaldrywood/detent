package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func authCredential(c hubapi.APICredential) hubauth.Credential {
	return hubauth.Credential{EntryKey: c.EntryKey, Hosted: c.Hosted, HostedRole: c.HostedRole, HostedMembership: c.HostedMembership, HostedPrincipal: c.HostedPrincipal, HostedKeyScope: c.HostedKeyScope, HostedKeyKind: c.HostedKeyKind, HostedProjectAccess: string(c.HostedProjectAccess), ManageRunners: c.ManageRunners, SessionHash: c.SessionHash, Hash: c.Hash, ID: c.ID, Name: c.Name, Scope: hubauth.TokenScope(c.Scope), NativeOnly: c.NativeOnly, Runner: c.Runner, Agent: c.Agent, RunnerRenewal: c.RunnerRenewal, RequestCapacity: c.RequestCapacity}
}
func nativeCredential(c hubauth.Credential) hubapi.APICredential {
	return hubapi.APICredential{EntryKey: c.EntryKey, Hosted: c.Hosted, HostedRole: c.HostedRole, HostedMembership: c.HostedMembership, HostedPrincipal: c.HostedPrincipal, HostedKeyScope: c.HostedKeyScope, HostedKeyKind: c.HostedKeyKind, HostedProjectAccess: hubapi.HostedProjectAccess(c.HostedProjectAccess), ManageRunners: c.ManageRunners, SessionHash: c.SessionHash, Hash: c.Hash, ID: c.ID, Name: c.Name, Scope: hubapi.APIScope(c.Scope), NativeOnly: c.NativeOnly, Runner: c.Runner, Agent: c.Agent, RunnerRenewal: c.RunnerRenewal, RequestCapacity: c.RequestCapacity}
}
func nativeCredentialResult(c hubauth.Credential, status int, err error) (hubapi.APICredential, int, error) {
	return nativeCredential(c), status, hubapi.FromFeatureError(err)
}
func (s *Service) requestAuth() *hubauth.Authenticator {
	deps := hubauth.Deps{
		TrustedProxy: s.config.TrustedProxy, GenerateToken: s.config.generateToken, Repository: s.config.TenantRepository, AccessRepository: s.config.AccessRepository, Organization: s.accessOrganization(), Now: s.config.now,
		Classify: operatorAuthorizationError, SharedSession: s.sharedHostedSession, CompleteInvitation: s.completePendingHostedInvitation,

		BoardCredential: func(ctx context.Context) (hubauth.Credential, bool) {
			credential, ok := entryBoardCredential(ctx)
			return authCredential(credential), ok
		},
		SharedClaims: hostedSharedClaims,

		RunnerEventRoute: runnerEventCommandRoute,
		RunnerEventContext: func(ctx context.Context) context.Context {
			return context.WithValue(ctx, runnerEventAuthorityKey{}, true)
		},
		StoreCredential: func(c echo.Context, credential hubauth.Credential) {
			c.Set("hub_api_credential", nativeCredential(credential))
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
func (s *Service) authInput(scope nativeScope) hubauth.Input {
	input := hubauth.Input{Organization: scope.organization, Project: scope.project, Credential: authCredential(scope.credential), Admin: scope.requireHostedAdmin, Action: scope.action, IntegrationRepository: scope.integrationRepository, IntegrationOwned: scope.integrationOwned}
	if scope.githubIntakeRevision != nil {
		input.IntegrationFence = func(ctx context.Context, r accessstore.Reader) error {
			if fence := scope.githubIntakeRevision; fence != nil {
				if fence.batchID != "" {
					owner, ok := r.(projectstore.IntakeBatchReader)
					if !ok {
						return accessstore.ErrDenied
					}
					batch, err := owner.ProjectIntakeBatch(ctx, scope.project)
					if err != nil {
						return err
					}
					if batch.ID != fence.batchID || batch.Revision != fence.revision || batch.Status != "discovering" && batch.Status != "importing" {
						return corestore.ErrRevisionConflict
					}
					if fence.authority == nil {
						return operatortool.ErrAccessDenied
					}
					reader, ok := r.(tenantstore.Reader)
					if !ok {
						return hubapi.ErrPostgresFeatureUnavailable
					}
					if err := s.gitHubBatchRequestAuthority(ctx, reader, *fence.authority, fence.requireAdminKey); err != nil {
						return err
					}
					if fence.reporter != nil {
						if err := s.requireGitHubBatchRunner(ctx, reader, *fence.reporter, fence.reporter.credential.Runner.RunnerID, scope.integrationRepository, s.config.now()); err != nil {
							return err
						}
					}

				}
				if fence.importID != "" {
					owner, ok := r.(projectstore.GitHubImportReader)
					if !ok {
						return accessstore.ErrDenied
					}
					job, err := owner.GitHubImport(ctx, scope.project, fence.importID)
					if err != nil {
						return err
					}
					if job.Revision != fence.revision {
						return corestore.ErrRevisionConflict
					}
				}
			}
			return nil
		}
	}
	return input
}

func nativeAuthInput(input hubauth.Input) nativeScope {
	return nativeScope{organization: input.Organization, project: input.Project, credential: nativeCredential(input.Credential), requireHostedAdmin: input.Admin, action: input.Action}
}
func (s *Service) operatorAuthOptions() hubauth.OperatorOptions {
	options := hubauth.OperatorOptions{
		Classify: operatorAuthorizationError,
		Error:    func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Refuse: func(c echo.Context, status int, code, message string, body any) error {
			return s.refusedAPIError(c, status, code, message, body)
		},
		AccountCredential: func(ctx context.Context, hash string) (hubauth.Credential, error) {
			c, err := s.hostedAccountCredential(ctx, hash)
			return authCredential(c), err
		},
		EntryOrigin: func(ctx context.Context, origin string) (hubauth.Credential, error) {
			c, err := s.currentEntryOriginCredential(ctx, origin)
			return authCredential(c), err
		},
		CurrentSession: s.WebSession, AccessTime: operatorAccessTime,
		Authority: func(ctx context.Context, c hubauth.Credential, org string) (operatortool.Authority, error) {
			return s.operatorCurrentAuthority(ctx, nativeCredential(c), org)
		},
		Bind: func(ctx context.Context, organization, origin string, c hubauth.Credential, resolve hubauth.Resolver) context.Context {
			nativeResolve := func(ctx context.Context) (hubapi.APICredential, error) {
				c, err := resolve(ctx)
				return nativeCredential(c), err
			}
			ctx = context.WithValue(ctx, operatorCredentialKey{}, billingAuthorization(nativeResolve))
			if c.EntryKey != nil {
				ctx = context.WithValue(ctx, operatorEntryOriginKey{}, origin)
			}
			ctx = context.WithValue(ctx, hubOperatorResolverKey{}, nativeResolve)
			return context.WithValue(ctx, nativeOperatorScopeKey{}, func(ctx context.Context) (nativeScope, error) {
				c, err := resolve(ctx)
				return nativeScope{organization: tracker.OrganizationID(organization), credential: nativeCredential(c)}, err
			})
		},
		Catalog: func(ctx context.Context, c hubauth.Credential, org string) context.Context {
			return s.withOperatorCatalog(ctx, nativeCredential(c), org)
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
		Classify: operatorAuthorizationError,
		Base: func(input hubauth.Input) operatortool.Authority {
			scope := nativeAuthInput(input)
			credential := scope.credential
			return operatortool.Authority{Explainer: operatorWorkReads{service: s, scope: scope}, WorkReads: operatorWorkReads{service: s, scope: scope}, Changes: hubChangeApplication{service: s, scope: scope}, BindContext: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, operatorScopeKey{}, scope)
				ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(context.Context) (hubapi.APICredential, error) { return credential, nil })
				ctx = context.WithValue(ctx, nativeOperatorScopeKey{}, func(context.Context) (nativeScope, error) { return scope, nil })
				return context.WithValue(ctx, operatorCredentialKey{}, billingAuthorization(func(context.Context) (hubapi.APICredential, error) { return credential, nil }))
			}}
		},
		KeyCredential: func(ctx context.Context, c hubauth.Credential) (hubauth.Credential, error) {
			v, err := s.hostedKeyCredentialFor(ctx, nativeCredential(c))
			return authCredential(v), err
		},
		RunnerAdministration: func(ctx context.Context, c hubauth.Credential) error {
			return s.requireHostedRunnerAdministration(ctx, nativeCredential(c))
		},
		RecheckMutation: func(ctx context.Context, input hubauth.Input) error {
			return s.recheckProjectRecordMutation(ctx, nativeAuthInput(input))
		},
		Resource: func(ctx context.Context, input hubauth.Input, r operatortool.Requirement) error {
			return s.checkOperatorResource(ctx, nil, nativeAuthInput(input), r)
		},
	}
}

func (s *Service) resolveHubauthNativeScope(ctx context.Context, c echo.Context) error {
	a := s.requestAuth()
	err := a.ResolveNative(ctx, c, hubauth.NativeOptions{
		Credential: func(c echo.Context) (hubauth.Credential, bool) {
			v, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			return authCredential(v), ok
		},
		EventScope: func(c echo.Context, input hubauth.Input) bool {
			return s.runnerEventCommandScope(c, nativeAuthInput(input))
		},
		ArtifactRead: artifactReadGrantRequest, RelayTicket: relayTicketRequest, LeaseRoute: runnerLeaseMutationRoute, RepositoryRoute: runnerRepositoryReportRoute,
		AuthorizeLease: func(ctx context.Context, input hubauth.Input) error {
			return s.authorizeRunnerLeaseProject(ctx, nativeAuthInput(input))
		},
		AuthorizeRepository: func(ctx context.Context, input hubauth.Input) error {
			return s.authorizeRunnerRepositoryProject(ctx, nativeAuthInput(input))
		},
		ReadProject: func(ctx context.Context, input hubauth.Input, write, models bool) error {
			record, err := s.readAuthorizedProjectRecord(ctx, nativeAuthInput(input), write, models)
			if err == nil {
				c.Set("native_project_record", record)
			}
			return err
		},
		ReadItem: func(ctx context.Context, input hubauth.Input, item string) error {
			_, _, err := s.readWorkItemRecord(ctx, nativeAuthInput(input), item)
			return err
		},
		RecordError: hubapi.ProjectRecordError,
		Store: func(c echo.Context, input hubauth.Input) {
			c.Set("native_scope", nativeAuthInput(input))
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
		Classify: operatorAuthorizationError,
		Error:    func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Refuse: func(c echo.Context, status int, code, message string, body any) error {
			return s.refusedAPIError(c, status, code, message, body)
		},
		IsNotFound:           func(err error) bool { return hubapi.IsNativeNotFound(hubapi.FromFeatureError(err)) },
		HostedAdministration: s.requireHostedAdministration,
		ArtifactPublisher: func(c echo.Context, v hubauth.Credential) bool {
			return s.hostedArtifactPublisher(c, nativeCredential(v))
		},
		ChangePrincipal: func(c echo.Context, v hubauth.Credential) bool {
			return s.hostedChangeCheckPrincipal(c, nativeCredential(v))
		},
		ConfigurationRank: configurationRankRequest, CompatibilityResource: s.requireCompatibilityResource,
		AdmitRunner: func(c echo.Context, v hubauth.Credential) error { return s.admitRunnerRequest(c, nativeCredential(v)) },
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
					return s.archiveRequestContext(c, nativeRequestScope(c))
				}, Audit: s.hostedAudit,
			}, roles...)(next)(c)
		}
	}
}
