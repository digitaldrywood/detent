package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/staticasset"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerLocalAppRoutes(e *echo.Echo) {
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", staticasset.New(detent.StaticFS()))))
	s.registerAppRoutes(e)
	e.GET(hostedOrganizationBase+"/project-rank", s.hostedProjectRank, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/project-rank", s.updateHostedProjectRank, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/integrations/slack", s.getSlackIntegration, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/integrations/slack", s.setSlackIntegration, s.localAppOnly)
	e.POST(hostedOrganizationBase+"/integrations/slack/test", s.testSlackIntegration, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/release-notes", s.releaseNotesAPI, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/release-notes/seen", s.releaseNotesAPI, s.localAppOnly)
	e.GET("/projects/:project/events", s.hostedEvents, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/projects", s.listHostedProjects, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/members", s.localAppMembers, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/name", s.hostedOrganizationName, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/name", s.renameHostedOrganization, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/model-selection", s.getOrganizationModelSelection, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/model-selection", s.updateOrganizationModelSelection, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/monthly-budget", s.organizationMonthlyBudgetScope, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/monthly-budget", s.organizationMonthlyBudgetScope, s.localAppOnly)
	for _, path := range []string{hostedOrganizationBase + "/slug", hostedOrganizationBase + "/projects/:project/slug"} {
		e.GET(path, s.slugSettings, s.localAppOnly)
		e.PUT(path, s.slugSettings, s.localAppOnly)
	}
	e.GET(hostedOrganizationBase+"/fleet", s.hostedFleet, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/secrets/:kind", s.organizationSpritesScope(s.projectSecretMetadata), s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/secrets/:kind", s.organizationSpritesScope(s.setProjectSecret), s.localAppOnly)
	e.DELETE(hostedOrganizationBase+"/secrets/:kind", s.organizationSpritesScope(s.removeProjectSecret), s.localAppOnly)
	e.GET(hostedOrganizationBase+"/sprite-pool", s.organizationSpritesScope(s.getSpritePool), s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/sprite-pool", s.organizationSpritesScope(s.setSpritePool), s.localAppOnly)
	for _, path := range []string{hostedOrganizationBase + "/work-view-preference", hostedOrganizationBase + "/projects/:project/work-view-preference"} {
		e.GET(path, s.workViewPreference, s.localAppOnly)
		e.PUT(path, s.workViewPreference, s.localAppOnly)
		e.DELETE(path, s.workViewPreference, s.localAppOnly)
	}
	e.GET(hostedOrganizationBase+"/work-views", s.savedWorkViews, s.localAppOnly)
	e.POST(hostedOrganizationBase+"/work-views", s.savedWorkViews, s.localAppOnly)
	e.GET(hostedOrganizationBase+"/work-views/:view", s.savedWorkViews, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/work-views/:view", s.savedWorkViews, s.localAppOnly)
	e.DELETE(hostedOrganizationBase+"/work-views/:view", s.savedWorkViews, s.localAppOnly)
	e.PUT(hostedOrganizationBase+"/work-views/:view/favorite", s.savedWorkViews, s.localAppOnly)
}

func (s *Service) localAppUnauthorized(c echo.Context, status int) error {
	if status == http.StatusUnauthorized {
		c.Response().Header().Set("WWW-Authenticate", `Basic realm="Detent Hub", charset="UTF-8"`)
	}
	return c.JSON(status, apiErrorResponse{Code: "unauthorized", Message: "A Hub administrator token is required"})
}

func (s *Service) localAppCredential(c echo.Context) (apiCredential, int, error) {
	return s.localAppCredentialContext(c.Request().Context(), c)
}

func (s *Service) localAppCredentialContext(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	if s.config.Hosted != nil {
		return apiCredential{}, http.StatusUnauthorized, errors.New("local authentication is unavailable")
	}
	token, err := apiBearerToken(c)
	if username, password, ok := c.Request().BasicAuth(); ok && username == "operator" {
		token, err = password, nil
	}
	if err != nil || len(token) > maxBearerTokenBytes || token == "" {
		return apiCredential{}, http.StatusUnauthorized, errors.New("local credentials are required")
	}
	credential, status, err := s.authenticateAPIToken(ctx, token, "", "")
	if err != nil {
		return apiCredential{}, status, err
	}
	if credential.Scope != apiScopeAdmin || credential.NativeOnly || credential.Runner.RunnerID != "" {
		return apiCredential{}, http.StatusForbidden, errors.New("local browser access requires an administrator")
	}
	if _, _, basic := c.Request().BasicAuth(); basic {
		if c.Request().Header.Get("Sec-Fetch-Site") == "cross-site" {
			return apiCredential{}, http.StatusForbidden, errors.New("cross-site browser access is unavailable")
		}
		if !hostedReadRequest(c) {
			origin, err := url.Parse(c.Request().Header.Get("Origin"))
			scheme := "http"
			if c.IsTLS() || s.config.TrustedProxy && c.Scheme() == "https" {
				scheme = "https"
			}
			if err != nil || origin.Scheme != scheme || origin.Host != c.Request().Host || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
				return apiCredential{}, http.StatusForbidden, errors.New("same-origin browser access is required")
			}
		}
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	credential.HostedRole = "admin"
	c.Set("hub_api_credential", credential)
	return credential, http.StatusOK, nil
}

func (s *Service) localAppBootstrap(c echo.Context) error {
	credential, status, err := s.localAppCredential(c)
	if err != nil {
		return s.localAppUnauthorized(c, status)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	payload := appBootstrap{
		Organizations: []appBootstrapOrganization{}, Projects: []appBootstrapProject{},
		Actor:     appBootstrapActor{PrincipalID: credential.ID, Subject: appSubject(credential), Email: credential.Name, Role: "admin", CanManage: true, CanManageRunners: true},
		CSRFToken: "local", SignInPath: "/login", Version: s.config.Version,
		Feature: s.appBootstrapFeature(), SelfHosted: true,
	}
	err = s.database.reader.QueryRowContext(ctx, "SELECT id,name,slug FROM organizations WHERE local=1").Scan(&payload.Organization.ID, &payload.Organization.Name, &payload.Organization.Slug)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.Organization.Current = true
	payload.Organization.Role = "admin"
	payload.Organizations = append(payload.Organizations, payload.Organization)
	payload.APIBase = "/api/v2/organizations/" + payload.Organization.ID
	payload.Projects, err = s.localAppProjects(ctx, credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload.Preferences, err = s.appBootstrapPreferences(ctx, payload.Organization.ID, payload.Projects)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, payload)
}

func (s *Service) appOrganizationID() string {
	if s.config.Hosted != nil {
		return s.config.Hosted.OrganizationID
	}
	return s.localOrganization
}

func (s *Service) appCredential(c echo.Context) (apiCredential, int, error) {
	return s.appCredentialContext(c.Request().Context(), c)
}

func (s *Service) appCredentialContext(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	if s.config.Hosted != nil {
		return s.hostedCredential(ctx, c)
	}
	return s.localAppCredentialContext(ctx, c)
}

func appSubject(credential apiCredential) string {
	if credential.Hosted != nil {
		return credential.Hosted.Subject
	}
	return "local_" + credential.ID
}

func (s *Service) localAppProjects(ctx context.Context, credential apiCredential) ([]appBootstrapProject, error) {
	projects := []appBootstrapProject{}
	rows, err := s.database.reader.QueryContext(ctx, "SELECT id,name,slug,profile,states_json FROM projects WHERE organization_id=? AND deleted_at IS NULL ORDER BY name,id", s.localOrganization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project appBootstrapProject
		var states string
		if err := rows.Scan(&project.ID, &project.Name, &project.Slug, &project.Profile, &states); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(states), &project.States); err != nil {
			return nil, err
		}
		if err := s.database.authorizeNativeProject(ctx, nativeScope{organization: tracker.OrganizationID(s.localOrganization), project: tracker.ProjectID(project.ID), credential: credential}); err != nil {
			return nil, err
		}
		icon, err := readProjectIcon(ctx, s.database.reader, nativeScope{organization: tracker.OrganizationID(s.localOrganization), project: tracker.ProjectID(project.ID)})
		if err != nil {
			return nil, err
		}
		project.Icon = projectIconOverride(icon)
		project.CanWrite, project.CanManageRunners = true, true
		projects = append(projects, project)
	}
	return projects, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) localAppOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.localAppCredential(c)
		if err != nil {
			return s.localAppUnauthorized(c, status)
		}
		if org := c.Param("organization"); org != "" && org != s.localOrganization {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if !hostedReadRequest(c) {
			now := formatHubTime(s.config.now())
			_, err := s.database.db.ExecContext(c.Request().Context(), `INSERT INTO hosted_members (user_id,email,membership_id,role,active,principal_id,created_at,updated_at) VALUES (?,?,?,'admin',1,?,?,?) ON CONFLICT(user_id) DO NOTHING`, appSubject(credential), credential.Name, "local_"+credential.ID, credential.ID, now, now)
			if err != nil {
				return s.nativeAPIError(c, err)
			}
		}
		return next(c)
	}
}

func (s *Service) localAppCreateProject(c echo.Context) error {
	var input struct {
		tracker.Mutation
		Name        string `json:"name"`
		GrantAccess bool   `json:"grant_access"`
	}
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if !input.GrantAccess {
		return s.nativeAPIError(c, nativeInvalid("Explicitly grant yourself project access"))
	}
	credential, status, err := s.localAppCredential(c)
	if err != nil {
		return s.localAppUnauthorized(c, status)
	}
	if c.Param("organization") != s.localOrganization {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(s.localOrganization), credential: credential}
	c.Set("native_scope", scope)
	request := createNativeProjectRequest{Mutation: input.Mutation, Name: input.Name, States: HostedProjectStates()}
	return s.nativeMutation(c, request.Mutation, request, s.createNativeProjectOperation(request))
}

func (s *Service) localAppMembers(c echo.Context) error {
	credential, status, err := s.localAppCredential(c)
	if err != nil {
		return s.localAppUnauthorized(c, status)
	}
	return c.JSON(http.StatusOK, hostedMembersResponse{
		Roles: []auth.OrganizationRole{}, MemberCount: 1,
		Members:     []hostedMemberView{{ID: appSubject(credential), UserID: appSubject(credential), Email: credential.Name, Role: "admin", Status: "active", Grants: []hostedMemberGrant{}}},
		Invitations: []hostedInvitationView{},
	})
}

func (s *Service) appAdministrator(c echo.Context) (apiCredential, error) {
	if s.config.Hosted == nil {
		credential, _, err := s.localAppCredential(c)
		return credential, err
	}
	return s.hostedAdministrator(c)
}
