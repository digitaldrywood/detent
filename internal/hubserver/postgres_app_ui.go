package hubserver

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) registerPostgresAppRoutes(e *echo.Echo) {
	if s.config.Hosted != nil {
		s.registerReadableRoutes(e)
		e.GET("/", s.hostedLanding)
		e.GET("/organization", s.hostedHome)
		e.POST("/projects", s.createHostedProject)
		e.POST("/organization/invite", s.inviteHostedMember)
		e.POST("/organization/members/:member/revoke", s.revokeHostedMember)
		e.POST("/organization/members/:member/role", s.changeHostedRole)
		e.POST("/organization/grants", s.changeHostedGrant)
		e.POST("/organization/billing/checkout", s.hostedBillingCheckout)
		e.POST("/organization/billing/portal", s.hostedBillingPortal)
		e.GET(hostedOrganizationBase+"/api-keys", s.hostedAPIKeys)
		e.POST(hostedOrganizationBase+"/api-keys", s.createHostedAPIKey)
		e.DELETE(hostedOrganizationBase+"/api-keys/:key", s.revokeHostedAPIKey)
		e.GET(hostedOrganizationBase+"/plan", s.hostedPlanJSON, s.hostedSessionOnly)
		e.POST(hostedOrganizationBase+"/switch", s.switchHostedOrganizationJSON, s.hostedSessionOnly)
		e.GET(hostedOrganizationBase+"/billing", s.hostedBillingJSON)
		e.POST(hostedOrganizationBase+"/billing/checkout", s.hostedBillingCheckout)
		e.POST(hostedOrganizationBase+"/billing/portal", s.hostedBillingPortal)
		s.registerWorkViewRoutes(e)
		e.GET(hostedOrganizationBase+"/projects", s.listHostedProjects, s.hostedSessionOnly)
		s.registerOrganizationSettingsRecordRoutes(e)
		e.GET(hostedOrganizationBase+"/logo", s.getOrganizationLogo, s.hostedSessionOnly)
		e.PUT(hostedOrganizationBase+"/logo", s.setOrganizationLogo, s.hostedAdministrationSession)
		e.GET(hostedOrganizationBase+"/attachment-metadata/:attachment", s.getOrganizationLogoAttachment, s.hostedSessionOnly)
		e.GET(hostedOrganizationBase+"/logo-metadata", s.getCurrentOrganizationLogoAttachment, s.hostedSessionOnly)
		s.registerOrganizationVisibleRoutes(e)
		s.registerHostedAccountPreferenceRoutes(e)
		s.registerHostedMemberAndSecretRoutes(e)
		s.registerOrganizationBudgetReadRoutes(e)
		s.registerMonthlyBudgetWriteRoutes(e)
		s.registerHostedCreditReadRoutes(e)
		s.registerHostedCreditWriteRoutes(e)
		e.GET("/daily-summary/unsubscribe", s.unsubscribeDailySummary)
		if s.config.Hosted.SharedEntry != nil {
			e.POST("/internal/v1/daily-summary/unsubscribe", s.unsubscribeDailySummary)
		}
		e.GET("/projects/:project/events", s.hostedEvents)
		e.GET(hostedOrganizationBase+"/activity", s.hostedActivity, s.operatorAuthority)
		e.GET(hostedOrganizationBase+"/fleet", s.hostedFleet, s.hostedSessionOnly)
		e.GET(hostedOrganizationBase+"/diagnostics", s.hostedDiagnostics, s.usageSessionOnly, s.operatorAuthority)
		e.GET(hostedOrganizationBase+"/diagnostics/requests", s.hostedRequestMetrics, s.operatorAuthority)
		e.GET(hostedOrganizationBase+"/reports", s.hostedReports, s.usageSessionOnly, s.operatorAuthority)
		e.GET(hostedOrganizationBase+"/reports/options", s.hostedReportOptions, s.usageSessionOnly, s.operatorAuthority)
		e.GET("/app/bootstrap", s.postgresAppBootstrapPayload)
		e.GET("/chat/bootstrap", s.postgresAppBootstrapPayload)
		e.GET("/app/updates", s.appUpdates)
	}
	e.Any("/*", s.postgresAppFallback)
}

func (s *Service) postgresAppFallback(c echo.Context) error {
	path := c.Request().URL.Path
	if appReserved(path) || path == "/internal" || strings.HasPrefix(path, "/internal/") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if s.config.Hosted == nil || s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return s.postgresFeatureUnavailable(c)
	}
	return s.postgresAppShell(c)
}

func (s *Service) postgresAppBootstrapPayload(c echo.Context) error {
	if s.config.Hosted == nil || s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return s.postgresFeatureUnavailable(c)
	}
	credential, status, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		if status >= http.StatusInternalServerError {
			return s.nativeAPIError(c, operatorAuthorizationError(err))
		}
		if status == http.StatusForbidden {
			return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "This account has no access to this organization", appBootstrapRefusal{Code: "forbidden", Message: "This account has no access to this organization", Details: appBootstrapRefusalDetails{Organization: s.config.Hosted.OrganizationID, CSRFToken: s.hostedPageCSRF(c)}})
		}
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	session, ok := c.Get("hosted_session").(auth.Session)
	if !ok {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	csrf := s.hostedPageCSRF(c)
	if csrf == "" {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	payload, err := s.readPostgresAppBootstrap(c.Request().Context(), credential, session)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.CSRFToken = csrf
	payload.Actor.PlatformRole = sharedPlatformRole(c)
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, payload)
}

func (s *Service) readPostgresAppBootstrap(ctx context.Context, credential apiCredential, session auth.Session) (appBootstrap, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	organizationID := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	manage := credential.HostedRole == "owner" || credential.HostedRole == "admin"
	payload := appBootstrap{
		Organization:  appBootstrapOrganization{ID: string(organizationID), PublicURL: s.config.Hosted.PublicURL, Current: true, Role: credential.HostedRole},
		Organizations: []appBootstrapOrganization{},
		Actor: appBootstrapActor{
			PrincipalID: operatorIdentity(credential, string(organizationID)).PrincipalID,
			Subject:     credential.Hosted.Subject,
			Email:       session.Email,
			Role:        credential.HostedRole,
			CanManage:   manage,
		},
		Projects:   []appBootstrapProject{},
		APIBase:    "/api/v2/organizations/" + string(organizationID),
		BasePath:   s.hostedBase(),
		SignInPath: s.hostedSignInPath(),
		Version:    s.config.Version,
	}
	if session.Identity != nil {
		if user, err := auth.LookupHostedUser(ctx, s.config.Hosted.Provider, session.Identity.Subject); err == nil {
			session.Identity.FirstName = user.FirstName
			session.Identity.LastName = user.LastName
			session.Identity.ProfilePictureURL = user.ProfilePictureURL
			payload.Actor.Email = user.Email
		}
		payload.Actor.FirstName = session.Identity.FirstName
		payload.Actor.LastName = session.Identity.LastName
		payload.Actor.ProfilePictureURL = session.Identity.ProfilePictureURL
	}
	if err := s.accessRead(ctx, organizationID, func(r tenantstore.Reader) error {
		organization, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		logo, err := r.OrganizationLogo(ctx)
		if err != nil {
			return err
		}
		if logo.ImageKey != "" {
			logo.URL = "/organizations/" + string(organizationID) + "/api/v2/attachments/" + logo.ImageKey
		}
		payload.Organization.Logo = &logo
		payload.Organization.Name = organization.Name
		payload.Organization.Slug = organization.Slug
		if s.database.plans() != nil {
			entitlement, err := s.database.hostedEntitlementRecord(ctx, r, s.config.now())
			if err != nil {
				return err
			}
			payload.Plan = &appBootstrapPlan{ID: entitlement.EffectiveBase.ID, Name: entitlement.Name, Source: entitlement.Source, WindowEndsAt: entitlement.WindowEndsAt.UTC().Format(time.RFC3339)}
		}
		return nil
	}); err != nil {
		return appBootstrap{}, fmt.Errorf("read PostgreSQL organization bootstrap: %w", err)
	}
	if payload.Organization.Slug == "" {
		return appBootstrap{}, errors.New("PostgreSQL organization slug is unavailable")
	}
	payload.Organization.PublicURL = strings.TrimRight(s.config.Hosted.PublicURL, "/") + "/" + payload.Organization.Slug + "/work"
	if identity := credential.Hosted; identity != nil && identity.SupportActor != "" {
		payload.Support = &appBootstrapSupport{Actor: identity.SupportActor, Reason: identity.SupportReason, ExpiresAt: identity.ExpiresAt.UTC().Format(time.RFC3339)}
	}
	projects, err := s.postgresAppProjects(ctx, credential)
	if err != nil {
		return appBootstrap{}, err
	}
	payload.Projects = projects
	if credential.HostedKeyScope == "" {
		payload.Organizations, err = s.hostedOrganizationChoices(ctx, session)
		if err != nil {
			return appBootstrap{}, err
		}
	}
	for index := range payload.Organizations {
		if payload.Organizations[index].Current {
			payload.Organizations[index].Name = payload.Organization.Name
			payload.Organizations[index].Slug = payload.Organization.Slug
			payload.Organizations[index].PublicURL = payload.Organization.PublicURL
		}
	}
	if credential.HostedRole != "viewer" {
		payload.Actor.CanManageRunners, err = s.appAllRunnerGrants(ctx, credential)
		if err != nil {
			return appBootstrap{}, err
		}
	}
	payload.Capabilities.Coordinator = s.conversations != nil && s.conversations.coordinator != nil && s.conversations.coordinator.Available()
	payload.Feature = s.appBootstrapFeature()
	payload.Feature.Workspaces = s.config.Workspace != nil && s.config.Workspace.Enabled && !s.config.CredentialMaintenance
	payload.Preferences = s.postgresAppBootstrapPreferences()
	return payload, nil
}

func (s *Service) postgresAppProjects(ctx context.Context, credential apiCredential) ([]appBootstrapProject, error) {
	const projectLimit = 200
	filter := projectAccessFilter(credential, "", "", projectLimit)
	var rows []accessstore.ProjectAccess
	var records []projectstore.Record
	var workspaceRows []runnerstore.ProjectWorkspaceAvailability
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		pageReader, ok := reader.(accessstore.ProjectAccessPageReader)
		if !ok {
			return ErrProjectParticipants
		}
		var err error
		rows, err = pageReader.ProjectAccessPage(ctx, filter)
		if err != nil {
			return err
		}
		if len(rows) > projectLimit {
			return corestore.ErrInvalidPage
		}
		records = make([]projectstore.Record, 0, len(rows))
		for _, row := range rows {
			record, err := reader.ProjectRecord(ctx, row.ProjectID)
			if err != nil {
				return err
			}
			records = append(records, record)
		}
		if s.config.Workspace != nil && s.config.Workspace.Enabled && !s.config.CredentialMaintenance && credential.HostedRole != "viewer" {
			workspaceReader, ok := reader.(runnerstore.ProjectWorkspaceReader)
			if !ok {
				return errors.New("project workspace reader is unavailable")
			}
			ids := make([]tracker.ProjectID, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ProjectID)
			}
			workspaceRows, err = workspaceReader.ProjectWorkspaceAvailability(ctx, ids, s.config.now())
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list PostgreSQL app projects: %w", err)
	}
	projects := make([]appBootstrapProject, 0, len(rows))
	for index, record := range records {
		project := appBootstrapProject{Icon: projectIntegrationRecord(record).Icon, ID: string(record.Core.ID), Name: record.Core.Name, Slug: record.Core.Slug, Profile: record.Core.Profile, States: record.States, CanWrite: rows[index].CanWrite, CanManageRunners: rows[index].ManageRunner}
		viewer := credential.HostedRole == "viewer"
		if credential.EntryKey != nil {
			project.CanWrite = project.CanWrite && hostedKeyAllows(credential.HostedKeyScope, apikey.ScopeWrite)
			project.CanManageRunners = project.CanManageRunners && hostedKeyAllows(credential.HostedKeyScope, apikey.ScopeAdmin)
		}
		project.CanWrite = project.CanWrite && !viewer
		project.CanManageRunners = project.CanManageRunners && !viewer
		for _, workspace := range workspaceRows {
			if string(workspace.ProjectID) != project.ID || !project.CanWrite {
				continue
			}
			project.Capabilities.Files, project.Capabilities.Exec = true, true
			terminal := workspace.Terminal && project.CanManageRunners && s.config.Workspace.Terminal.Enabled && terminalIsolationAllowed([]string{workspacesession.CapabilityTerminal}, s.config.Workspace.Terminal.Isolation, workspace.Isolation)
			if s.config.Workspace.Terminal.Isolation == workspacesession.IsolationUser && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				terminal = false
			}
			project.Capabilities.Terminal = project.Capabilities.Terminal || terminal
		}
		projects = append(projects, project)
	}
	return projects, nil
}

func (s *Service) postgresAppBootstrapPreferences() appBootstrapPreferences {
	preferences := appBootstrapPreferences{Models: []appBootstrapChoice{}, Efforts: []appBootstrapChoice{}, Access: []appBootstrapChoice{}}
	if s.conversations == nil {
		return preferences
	}
	configured := s.conversationDefaultModel()
	preferences.Models = append(preferences.Models, appBootstrapChoice{ID: conversation.PreferenceAuto, Label: "Auto", Default: configured == ""})
	if configured != "" {
		preferences.Models = append(preferences.Models, appBootstrapChoice{ID: configured, Label: configured, Default: true})
	}
	preferences.Efforts = appBootstrapChoices(conversation.ReasoningEfforts()[1:], conversationChoiceLabel, strings.TrimSpace(s.conversations.config.ReasoningEffort))
	preferences.Access = appBootstrapChoices(conversation.AccessLevels()[1:], conversationChoiceLabel, "")
	return preferences
}

func (s *Service) postgresAppShell(c echo.Context) error {
	path := c.Request().URL.Path
	if !hostedReadRequest(c) || s.config.Hosted == nil || s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if path == "/login" {
		return s.servePostgresAppShell(c, path)
	}
	if _, _, err := s.hostedSession(c.Request().Context(), c); err != nil {
		if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			return s.nativeAPIError(c, classified)
		}
		if first := strings.Split(strings.Trim(path, "/"), "/")[0]; readableurl.Validate(first) == nil && strings.Contains(strings.Trim(path, "/"), "/") {
			return s.nativeAPIError(c, nativeNotFound())
		}
		return c.Redirect(http.StatusSeeOther, s.hostedSignInPath())
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		credential, _, err := s.hostedCredential(c.Request().Context(), c)
		if err != nil {
			if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
				return s.nativeAPIError(c, classified)
			}
			return s.nativeAPIError(c, nativeNotFound())
		}
		organizationID := tracker.OrganizationID(s.config.Hosted.OrganizationID)
		var project projectstore.Record
		err = s.config.TenantRepository.ReadTenant(c.Request().Context(), organizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			project, err = r.ProjectRecord(c.Request().Context(), tracker.ProjectID(parts[1]))
			if errors.Is(err, corestore.ErrNotFound) {
				project, err = r.ProjectBySlug(c.Request().Context(), parts[1])
			}
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			return nil
		})
		if err != nil {
			if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
				return s.nativeAPIError(c, classified)
			}
			return s.nativeAPIError(c, nativeNotFound())
		}
		scope := nativeScope{organization: organizationID, project: project.Core.ID, credential: credential}
		if err := s.authorizeProjectRecord(c.Request().Context(), scope, false); err != nil {
			if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
				return s.nativeAPIError(c, classified)
			}
			return s.nativeAPIError(c, nativeNotFound())
		}
	}
	if target, handled, err := s.readableShellPath(c); err != nil {
		if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			return s.nativeAPIError(c, classified)
		}
		return s.nativeAPIError(c, nativeNotFound())
	} else {
		original := c.Request().URL.Path
		if organization, ok := c.Get("readable_organization_slug").(string); ok {
			original = "/" + organization + original
		}
		if handled && target != original {
			if query := c.Request().URL.RawQuery; query != "" {
				target += "?" + query
			}
			return c.Redirect(http.StatusPermanentRedirect, target)
		}
	}
	return s.servePostgresAppShell(c, path)
}

func (s *Service) servePostgresAppShell(c echo.Context, path string) error {
	content, err := fs.ReadFile(s.config.staticFiles(), conversationClientShell)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.config.Logger.Error("application client shell could not be read", "error", err)
		}
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "client_unavailable", Message: "The application client is not built on this hub"})
	}
	c.Response().Header().Set("Cache-Control", "no-cache")
	shell := conversationClientShellFor(content, s.hostedBase(), s.hostedSignInPath())
	firstSegment := strings.Split(strings.Trim(path, "/"), "/")[0]
	_, readableShared := c.Get("readable_organization_slug").(string)
	if path != "/login" && (readableShared || readableurl.Validate(firstSegment) == nil) {
		organization, err := s.readPostgresAccessOrganization(c.Request().Context())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		shell = []byte(strings.Replace(string(shell), "<head>", `<head><meta name="detent-router-base" content="/`+html.EscapeString(organization.Slug)+`">`, 1))
	}
	return c.HTMLBlob(http.StatusOK, shell)
}

func (s *Service) readPostgresAccessOrganization(ctx context.Context) (accessstore.Organization, error) {
	var organization accessstore.Organization
	err := s.accessRead(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), func(r tenantstore.Reader) error {
		var err error
		organization, err = r.AccessOrganization(ctx)
		return err
	})
	return organization, err
}
