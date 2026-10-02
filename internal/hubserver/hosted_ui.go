package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) registerHostedRoutes(e *echo.Echo) {
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", http.FileServerFS(detent.StaticFS()))))
	e.GET("/", s.hostedLanding)
	e.GET("/login", s.appShell)
	e.GET("/auth/oidc/start", s.startHostedLogin)
	e.GET("/auth/oidc/callback", s.completeHostedLogin)
	e.GET("/invite", s.startHostedInvitation)
	e.POST("/logout", s.logoutHosted)
	e.POST("/support/start", s.startHostedSupport)
	e.GET("/support", s.hostedSupportPage)
	e.GET(hostedOrganizationBase+"/api-keys", s.hostedAPIKeys)
	e.POST(hostedOrganizationBase+"/api-keys", s.createHostedAPIKey)
	e.DELETE(hostedOrganizationBase+"/api-keys/:key", s.revokeHostedAPIKey)
	e.GET("/organization", s.hostedHome)
	e.GET("/organization/plan", s.hostedPlanPage)
	e.GET("/organization/billing", s.hostedBillingPage)
	e.POST("/organization/billing/checkout", s.hostedBillingCheckout)
	e.POST("/organization/billing/portal", s.hostedBillingPortal)
	e.POST("/organization/billing/credits/checkout", s.hostedCreditCheckout)
	e.POST("/organization/billing/credits/auto-fund", s.hostedCreditAutoFund)
	e.GET("/api/v2/organizations/:organization/billing", s.hostedBillingJSON)
	e.POST("/api/v2/organizations/:organization/billing/checkout", s.hostedBillingCheckout)
	e.POST("/api/v2/organizations/:organization/billing/portal", s.hostedBillingPortal)
	e.POST("/api/v2/organizations/:organization/billing/credits/checkout", s.hostedCreditCheckout)
	e.PUT("/api/v2/organizations/:organization/billing/credits/auto-fund", s.hostedCreditAutoFund)
	e.POST("/webhooks/stripe", s.hostedStripeWebhook)
	e.GET("/api/cloud/billing/subscription", s.hostedBillingExport)
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
	e.GET("/api/v2/organizations/:organization/entitlements", s.hostedPlanReport)
	e.POST("/api/v2/organizations/:organization/entitlements", s.updateHostedPlan)
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
	if _, _, err := s.hostedCredential(c); err == nil {
		return s.appShell(c)
	}
	return s.hostedHome(c)
}

func (s *Service) hostedHome(c echo.Context) error {
	session, _, err := s.hostedSession(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, s.hostedSignInPath())
	}
	data := templates.HostedPageData{Mode: "onboarding", Title: "Your organization", Email: session.Email}
	setup, err := s.hostedAccountSetupFor(c.Request().Context(), session)
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization information is temporarily unavailable")
	}
	data.CanCreate, data.CanSupport = setup.CanCreate, setup.CanSupport
	if hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) && session.Identity.SupportActor == "" {
		data.Mode = "organization"
		data.Notice = "Staff access is limited to account and usage metadata. Customer content requires authorized temporary support access."
		return s.renderHosted(c, http.StatusOK, data)
	}
	credential, _, accessErr := s.hostedCredential(c)
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

func (s *Service) hostedPageData(c echo.Context, credential apiCredential, data *templates.HostedPageData) error {
	identity := credential.Hosted
	if identity.SupportActor != "" {
		data.SupportActor, data.SupportReason = identity.SupportActor, identity.SupportReason
		data.SupportExpiry = identity.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT name FROM organizations WHERE id = ?", s.config.Hosted.OrganizationID).Scan(&data.OrganizationName); err != nil {
		return err
	}
	rows, err := s.database.db.QueryContext(c.Request().Context(), `SELECT p.id,p.name,g.manage_runner FROM projects p JOIN hosted_project_grants g ON g.project_id = p.id WHERE g.user_id = ? AND p.organization_id = ? ORDER BY p.id`, identity.Subject, s.config.Hosted.OrganizationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var project templates.HostedProjectChoice
		var runner bool
		if err := rows.Scan(&project.ID, &project.Name, &runner); err != nil {
			return errors.Join(err, rows.Close())
		}
		data.Projects = append(data.Projects, project)
		data.CanManageRunners = data.CanManageRunners || runner
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !data.CanManage {
		return nil
	}
	members, err := s.config.Hosted.Provider.Memberships(c.Request().Context(), "", identity.OrganizationID)
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
	initial, status, err := s.hostedCredential(c)
	if err != nil {
		return c.NoContent(status)
	}
	initialScope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(c.Param("project")), credential: initial}
	if err := s.requireHostedProject(c.Request().Context(), s.database.db, initialScope, false); err != nil {
		return c.NoContent(http.StatusForbidden)
	}
	workspaceID := c.QueryParam("workspace")
	if workspaceID != "" {
		if _, err := s.readWorkspace(c.Request().Context(), initialScope, workspaceID); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if err := s.hostedAudit(c.Request().Context(), initial.Hosted, "action", "GET /projects/:project/events", string(initialScope.project), http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var workspaceRevision int64
	for s.ready.Load() {
		credential, _, err := s.hostedCredential(c)
		if err != nil {
			return nil
		}
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(c.Param("project")), credential: credential}
		observation, err := s.readHostedEventObservation(c.Request().Context(), scope, workspaceID)
		if err != nil {
			return nil
		}
		if _, err := fmt.Fprintf(c.Response(), "event: activity\ndata: %d\n\n", observation.Sequence); err != nil {
			return nil
		}
		if record := observation.Workspace; record != nil && record.Revision != workspaceRevision {
			resource, err := json.Marshal(record)
			if err != nil {
				return nil
			}
			if _, err := fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", workspacesession.EventType(record.State), resource); err != nil {
				return nil
			}
			workspaceRevision = record.Revision
		}
		c.Response().Flush()
		select {
		case <-c.Request().Context().Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}

func (s *Service) hostedMetadata(c echo.Context) error {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		credential, _, err := s.authenticateAPIRequest(c)
		if err != nil || credential.ID != bootstrapTokenID || credential.Scope != apiScopeAdmin {
			return c.NoContent(http.StatusForbidden)
		}
	} else {
		session, _, err := s.hostedSession(c)
		if err != nil || session.Identity.SupportActor != "" || !hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
			return c.NoContent(http.StatusForbidden)
		}
	}
	report, err := s.database.hostedMetadata(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "metadata_unavailable", Message: "Service metadata is temporarily unavailable"})
	}
	usage, err := s.hostedUsage(c.Request().Context(), report)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, usage)
}

type hostedUsageReport struct {
	CostDrivers hostedCostDrivers `json:"cost_drivers"`
	Entitlement HostedEntitlement `json:"entitlement"`
	HostedMetadata
	PlanID            string `json:"plan_id"`
	StorageQuotaBytes int64  `json:"storage_quota_bytes,omitempty"`
	EventQuota        int64  `json:"event_quota,omitempty"`
}

func (s *Service) hostedUsage(ctx context.Context, report HostedMetadata) (hostedUsageReport, error) {
	entitlement, err := s.database.hostedPlanUsage(ctx, s.config.now())
	if err != nil {
		return hostedUsageReport{}, err
	}
	drivers, err := s.hostedCostDrivers(ctx, entitlement)
	return hostedUsageReport{CostDrivers: drivers, HostedMetadata: report, Entitlement: entitlement, PlanID: entitlement.EffectiveBase.ID, StorageQuotaBytes: entitlement.Allowances["collaboration_bytes"], EventQuota: entitlement.Allowances["ingested_events"]}, err
}

func (s *Service) hostedBilling(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c)
	if err != nil {
		return s.nativeAPIError(c, auth.ErrHostedIdentity)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "billing_viewed", "GET /api/cloud/billing", "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.database.hostedMetadata(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	usage, err := s.hostedUsage(c.Request().Context(), report)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, usage)
}

func hostedFormTrue(c echo.Context, key string) bool {
	return strings.EqualFold(c.FormValue(key), "true")
}

type hostedCostDrivers struct {
	ActiveIssues          int64            `json:"active_issues"`
	ArchivedIssues        int64            `json:"archived_issues"`
	DatabaseBytes         *int64           `json:"database_bytes"`
	WALBytes              *int64           `json:"wal_bytes"`
	Requests              *int64           `json:"requests"`
	RequestBytes          *int64           `json:"request_bytes"`
	ResponseBytes         *int64           `json:"response_bytes"`
	ArtifactRetainedBytes *int64           `json:"artifact_retained_bytes"`
	ArtifactReservedBytes *int64           `json:"artifact_reserved_bytes"`
	RelayBytes            *int64           `json:"relay_bytes"`
	WindowEndsAt          time.Time        `json:"window_ends_at"`
	OperatorAI            chatUsageSummary `json:"operator_ai"`
	OperatorAICostUSD     *float64         `json:"operator_ai_cost_usd"`
}

func (s *Service) hostedCostDrivers(ctx context.Context, entitlement HostedEntitlement) (hostedCostDrivers, error) {
	d := s.database
	result := hostedCostDrivers{WindowEndsAt: entitlement.WindowEndsAt}
	if err := d.db.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN i.archived=0 THEN 1 ELSE 0 END),0),coalesce(sum(CASE WHEN i.archived=1 THEN 1 ELSE 0 END),0) FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.organization_id=? AND p.profile='native'`, d.hostedOrganization).Scan(&result.ActiveIssues, &result.ArchivedIssues); err != nil {
		return result, err
	}
	for _, file := range []struct {
		suffix string
		value  **int64
	}{{"", &result.DatabaseBytes}, {"-wal", &result.WALBytes}} {
		info, err := os.Stat(d.path + file.suffix)
		if err == nil {
			bytes := info.Size()
			*file.value = &bytes
		} else if file.suffix == "-wal" && errors.Is(err, os.ErrNotExist) {
			zero := int64(0)
			*file.value = &zero
		}
	}
	if requests, known := entitlement.Usage["http_requests"]; known {
		result.Requests = &requests
		response := entitlement.Usage["http_response_bytes"]
		result.ResponseBytes = &response
		if entitlement.Usage["http_request_bytes_known"] == requests {
			bytes := entitlement.Usage["http_request_bytes"]
			result.RequestBytes = &bytes
		}
	}
	var artifacts int64
	if err := d.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_artifact_usage WHERE observed_at > 0").Scan(&artifacts); err != nil {
		return result, err
	}
	if artifacts > 0 {
		retained, reserved := entitlement.Usage["artifact_retained_bytes"], entitlement.Usage["artifact_reserved_bytes"]
		result.ArtifactRetainedBytes, result.ArtifactReservedBytes = &retained, &reserved
	}
	if relay, known := entitlement.Usage["relay_bytes"]; known {
		result.RelayBytes = &relay
	}
	var err error
	result.OperatorAI, err = d.chatUsageSummary(ctx, d.hostedOrganization, chatBillingWindow(s.config.now(), billing.Snapshot{}), nil)
	if err != nil {
		return result, err
	}
	if result.OperatorAI.UnpricedTurns == 0 {
		cost := result.OperatorAI.CostUSD
		result.OperatorAICostUSD = &cost
	}
	return result, nil
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
	if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_members").Scan(&members); err != nil {
		return hostedAccountSetup{}, err
	}
	staff := hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) && session.Identity.SupportActor == ""
	return hostedAccountSetup{Email: session.Email, CanCreate: members == 0 && session.Identity.Subject == s.config.Hosted.BootstrapSubject && session.Identity.SupportActor == "" && !staff, CanSupport: session.Identity.SupportActor == "" && hostedEmailListed(s.config.Hosted.SupportActors, session.Email), Staff: staff}, nil
}
