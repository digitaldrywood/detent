package hubserver

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/staticasset"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Service) registerHostedRoutes(e *echo.Echo) {
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", staticasset.New(staticFiles(s.config.StaticFS)))))
	e.GET("/", s.hostedLanding)
	e.GET("/login", s.appShell)
	e.GET("/auth/oidc/start", s.startHostedLogin)
	e.GET("/auth/oidc/callback", s.completeHostedLogin)
	e.GET("/invite", s.startHostedInvitation)
	e.GET("/issue-notifications/unsubscribe", s.unsubscribeIssueEmail)
	s.registerDailySummaryUnsubscribeRoute(e)
	e.POST("/logout", s.logoutHosted)
	e.POST("/support/start", s.startHostedSupport)
	e.GET("/support", s.hostedSupportPage)
	e.GET(hostedOrganizationBase+"/api-keys", s.hostedAPIKeys)
	e.POST(hostedOrganizationBase+"/api-keys", s.createHostedAPIKey)
	e.DELETE(hostedOrganizationBase+"/api-keys/:key", s.revokeHostedAPIKey)
	e.GET("/organization", s.hostedHome)
	e.GET("/organization/billing", s.hostedBillingPage)
	e.POST("/organization/billing/checkout", s.hostedBillingCheckout)
	e.POST("/organization/billing/portal", s.hostedBillingPortal)
	e.GET("/api/v2/organizations/:organization/billing", s.hostedBillingJSON)
	e.POST("/api/v2/organizations/:organization/billing/checkout", s.hostedBillingCheckout)
	e.POST("/api/v2/organizations/:organization/billing/portal", s.hostedBillingPortal)
	s.registerHostedCreditWriteRoutes(e)
	e.GET("/api/cloud/billing/subscription", s.hostedBillingExport)
	s.registerHostedCreditReadRoutes(e)
	e.POST("/organization/create", s.createHostedOrganization)
	e.POST("/organization/switch", s.switchHostedOrganization)
	e.POST("/organization/invite", s.inviteHostedMember)
	e.POST("/organization/members/:member/revoke", s.revokeHostedMember)
	e.POST("/organization/members/:member/role", s.changeHostedRole)
	e.POST("/organization/grants", s.changeHostedGrant)
	e.POST("/projects", s.createHostedProject)
	e.GET("/projects/:project/events", s.hostedEvents)
	e.GET("/api/cloud/metadata", s.hostedMetadata)
	e.GET("/api/cloud/billing", s.hostedBilling)
	hostedplans.RegisterRoutes(e.Group(""), s.hostedPlansHTTPDeps())
	e.POST("/api/v2/organizations/:organization/artifact-allowances/:service", s.hostedArtifactAllowances)
	s.registerHostedUsageRoutes(e)
	s.registerHostedOrganizationRoutes(e)
	s.registerAppRoutes(e)
}

func (s *Service) renderHosted(c echo.Context, status int, data templates.HostedPageData) error {
	data.OrganizationID = s.config.Hosted.OrganizationID
	data.Base, data.SharedOrigin = s.hostedBase(), s.hostedShared()
	if data.SharedOrigin {
		data.CanSupport = false
	}
	data.Assets.Favicon = "/static/img/detent-mark.svg"
	if data.OrganizationName == "" {
		data.OrganizationName = data.OrganizationID
	}
	if session, ok := c.Get("hosted_session").(auth.Session); ok {
		data.Email = session.Email
		if session.Identity != nil && session.Identity.SupportActor != "" {
			data.SupportActor, data.SupportReason = session.Identity.SupportActor, session.Identity.SupportReason
			data.SupportExpiry = session.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	data.CSRF = s.hostedPageCSRF(c)
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(status)
	return templates.HostedPage(data).Render(c.Request().Context(), c.Response())
}

func (s *Service) hostedPageCSRF(c echo.Context) string {
	if s.hostedShared() {
		return s.hostedSharedCSRF(c)
	}
	if cookie, err := c.Cookie(hostedCookie); err == nil {
		return hostedCSRF(cookie.Value)
	}
	return ""
}

func (s *Service) hostedError(c echo.Context, status int, message string) error {
	return s.renderHosted(c, status, templates.HostedPageData{Mode: "denied", Title: "Access unavailable", Error: message})
}

func (s *Service) hostedDenied(c echo.Context, status int, message string, denial auth.HostedDenial) error {
	denial.Status = status
	auth.LogHostedDenial(s.hostedAuthLogger, c.Response(), c.Request(), denial)
	return s.hostedError(c, status, message)
}

// hostedLanding serves the root. A member, or a support session acting as
// one, gets the client application; a session with no organization access
// still gets the organization page, which carries the chooser, create and
// invitation guidance and the staff notice.
func (s *Service) hostedLanding(c echo.Context) error {
	if _, _, err := s.hostedCredential(c.Request().Context(), c); err == nil {
		if s.database.postgres != nil {
			return s.postgresAppShell(c)
		}
		return s.appShell(c)
	}
	return s.hostedHome(c)
}

func (s *Service) hostedHome(c echo.Context) error {
	session, _, err := s.hostedSession(c.Request().Context(), c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, s.hostedSignInPath())
	}
	data := templates.HostedPageData{Mode: "onboarding", Title: "Your organization", Email: session.Email}
	setup, err := s.hostedAccountSetupFor(c.Request().Context(), session)
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization information is temporarily unavailable")
	}
	data.CanCreate, data.CanSupport = setup.CanCreate, setup.CanSupport
	if s.config.Hosted.SharedEntry == nil && hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) && session.Identity.SupportActor == "" {
		data.Mode = "organization"
		data.Notice = "Staff access is limited to account and usage metadata. Customer content requires authorized temporary support access."
		return s.renderHosted(c, http.StatusOK, data)
	}
	credential, _, accessErr := s.hostedCredential(c.Request().Context(), c)
	if accessErr != nil {
		data.Notice = "Create your reserved organization, join with an invitation token, or switch to an organization you belong to."
	} else {
		data.Mode = "organization"
		data.CanManage = credential.HostedRole == "owner" || credential.HostedRole == "admin"
		data.CanManageOwnership = credential.HostedRole == "owner"
		data.CanCreate = data.CanManage
		if err := s.hostedPageData(c, credential, &data); err != nil {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization information is temporarily unavailable")
		}
	}
	if session.Identity.SupportActor == "" && !s.hostedShared() {
		memberships, err := s.config.Hosted.Provider.Memberships(c.Request().Context(), session.Identity.Subject, "")
		if err != nil {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization membership is temporarily unavailable")
		}
		for _, destination := range s.config.Hosted.Directory {
			for _, membership := range memberships {
				if membership.OrganizationID == destination.WorkOSOrganizationID && membership.UserID == session.Identity.Subject && membership.Status == "active" {
					data.Organizations = append(data.Organizations, templates.HostedOrganizationChoice{ID: destination.OrganizationID, Name: destination.OrganizationID})
				}
			}
		}
	}
	return s.renderHosted(c, http.StatusOK, data)
}

func (s *Service) hostedPageData(c echo.Context, credential hubapi.APICredential, data *templates.HostedPageData) error {
	ctx := c.Request().Context()
	identity := credential.Hosted
	if identity.SupportActor != "" {
		data.SupportActor, data.SupportReason = identity.SupportActor, identity.SupportReason
		data.SupportExpiry = identity.ExpiresAt.UTC().Format(time.RFC3339)
	}
	organization, err := s.readPostgresAccessOrganization(ctx)
	if err != nil {
		return err
	}
	data.OrganizationName = organization.Name
	projects, err := s.postgresAppProjects(ctx, credential)
	if err != nil {
		return err
	}
	for _, project := range projects {
		data.Projects = append(data.Projects, templates.HostedProjectChoice{ID: project.ID, Name: project.Name})
		data.CanManageRunners = data.CanManageRunners || project.CanManageRunners
	}
	if !data.CanManage {
		return nil
	}
	members, err := s.config.Hosted.Provider.Memberships(ctx, "", identity.OrganizationID)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.OrganizationID == identity.OrganizationID && member.Status == "active" {
			data.Members = append(data.Members, templates.HostedMember{ID: member.ID, UserID: member.UserID, Role: member.Role.Slug})
		}
	}
	return nil
}

func (s *Service) hostedEvents(c echo.Context) error {
	initial, status, err := s.appCredential(c)
	if err != nil {
		if status == http.StatusForbidden {
			return s.hostedJSONError(c, status, "Hosted credential is not authorized for project events")
		}
		return c.NoContent(status)
	}
	initialScope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID()), Project: tracker.ProjectID(c.Param("project")), Credential: initial}
	if err := s.requireHostedProject(c.Request().Context(), nil, initialScope, false); err != nil {
		if hubapi.IsNativeNotFound(err) {
			return s.nativeAPIError(c, err)
		}
		return s.hostedJSONError(c, http.StatusForbidden, "Project access is required for events")
	}
	if err := s.requireBoardRecordProject(c.Request().Context(), initialScope); err != nil {
		return s.nativeAPIError(c, err)
	}
	workspaceID := c.QueryParam("workspace")
	workItemID := c.QueryParam("work_item")
	if workItemID != "" {
		readCtx := c.Request().Context()
		err := s.config.TenantRepository.ReadTenant(readCtx, initialScope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			return requireDashboardRecordItem(readCtx, r, initialScope, workItemID)
		})
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if workspaceID != "" {
		if _, err := s.readWorkspace(c.Request().Context(), initialScope, workspaceID); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if err := s.hostedAudit(c.Request().Context(), initial.Hosted, "action", "GET /projects/:project/events", string(initialScope.Project), http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	board := c.QueryParam("board") == "true"
	if board {
		if workspaceID != "" || workItemID != "" {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("board stream cannot include a workspace or item"))
		}
		params := c.Request().URL.Query()
		params.Del("board")
		params.Del("since")
		if err := hubrecords.ValidateNativeIssueQuery(params); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	value := c.Request().Header.Get("Last-Event-ID")
	if value == "" {
		value = c.QueryParam("since")
	}
	var sequence int64
	if value != "" {
		sequence, err = strconv.ParseInt(value, 10, 64)
		if err != nil || sequence < 0 {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("Stream sequence is invalid"))
		}
	}
	return s.serveDashboardEvents(c, initialScope, sequence, value == "", board, workspaceID, workItemID)
}

func (s *Service) hostedMetadata(c echo.Context) error {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		credential, _, err := s.authenticateAPIRequest(c)
		if err != nil || credential.ID != bootstrapTokenID || credential.Scope != hubapi.APIScopeAdmin {
			return s.hostedJSONError(c, http.StatusForbidden, "Bootstrap administrator access is required for service metadata")
		}
	} else {
		session, _, err := s.hostedSession(c.Request().Context(), c)
		if err != nil || session.Identity == nil || session.Identity.SupportActor != "" || !hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) {
			return s.hostedJSONError(c, http.StatusForbidden, "Staff access is required for service metadata")
		}
	}
	report, err := s.readHostedMetadata(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, hubapi.APIErrorResponse{Code: "metadata_unavailable", Message: "Service metadata is temporarily unavailable"})
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) hostedBilling(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c.Request().Context(), c)
	if err != nil {
		return s.nativeAPIError(c, auth.ErrHostedIdentity)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "billing_viewed", "GET /api/cloud/billing", "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.readHostedMetadata(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func hostedFormTrue(c echo.Context, key string) bool {
	return strings.EqualFold(c.FormValue(key), "true")
}

// hostedAccountSetupFor shares account/onboarding facts with the typed session
// projection. It never carries the browser's form or provider exchange secrets.
type hostedAccountSetup struct {
	Email      string `json:"email"`
	CanCreate  bool   `json:"can_create"`
	CanSupport bool   `json:"can_support"`
	Staff      bool   `json:"staff"`
}

func (s *Service) hostedAccountSetupFor(ctx context.Context, session auth.Session) (hostedAccountSetup, error) {
	var members int
	if err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		rows, err := r.AccessMembers(ctx, "", 1)
		members = len(rows)
		return err
	}); err != nil {
		return hostedAccountSetup{}, err
	}
	staff := s.config.Hosted.SharedEntry == nil && hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) && session.Identity.SupportActor == ""
	return hostedAccountSetup{Email: session.Email, CanCreate: members == 0 && session.Identity.Subject == s.config.Hosted.BootstrapSubject && session.Identity.SupportActor == "" && (s.config.Hosted.SharedEntry != nil || !staff), CanSupport: session.Identity.SupportActor == "" && hostedplans.EmailListed(s.config.Hosted.SupportActors, session.Email), Staff: staff}, nil
}
