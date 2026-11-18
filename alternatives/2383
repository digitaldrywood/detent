package hubserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/staticasset"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerLocalAppRoutes(e *echo.Echo) {
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", staticasset.New(s.config.staticFiles()))))
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
	e.GET(hostedOrganizationBase+"/work-board", s.workBoard, s.requireAPIScope(hubapi.APIScopeOperator))
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
	return c.JSON(status, hubapi.APIErrorResponse{Code: "unauthorized", Message: "A Hub administrator token is required"})
}

func (s *Service) localAppCredential(c echo.Context) (hubapi.APICredential, int, error) {
	return s.localAppCredentialContext(c.Request().Context(), c)
}

func (s *Service) localAppCredentialContext(ctx context.Context, c echo.Context) (hubapi.APICredential, int, error) {
	return hubscope.NativeCredentialResult(s.requestAuth().LocalCredential(ctx, c))
}

func (s *Service) localAppBootstrap(c echo.Context) error {
	credential, status, err := s.localAppCredential(c)
	if err != nil {
		return s.localAppUnauthorized(c, status)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	payload := appBootstrap{
		Organizations: []appBootstrapOrganization{}, Projects: []appBootstrapProject{},
		Actor:     appBootstrapActor{PrincipalID: credential.ID, Subject: appSubject(credential), Email: credential.Name, Role: "admin", CanManage: true, CanManageRunners: true},
		CSRFToken: "local", SignInPath: "/login", Version: s.config.Version,
		Feature: s.appBootstrapFeature(), SelfHosted: true,
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.localOrganization), Credential: credential, RequireHostedAdmin: true}
	err = s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		organization, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		if !organization.Local {
			return hubapi.NativeNotFound()
		}
		payload.Organization.ID = string(organization.OrganizationID)
		payload.Organization.Name = organization.Name
		payload.Organization.Slug = organization.Slug
		return nil
	})
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

func (s *Service) appCredential(c echo.Context) (hubapi.APICredential, int, error) {
	return s.appCredentialContext(c.Request().Context(), c)
}

func (s *Service) appCredentialContext(ctx context.Context, c echo.Context) (hubapi.APICredential, int, error) {
	if s.config.Hosted != nil {
		return s.hostedCredential(ctx, c)
	}
	return s.localAppCredentialContext(ctx, c)
}

func appSubject(credential hubapi.APICredential) string {
	if credential.Hosted != nil {
		return credential.Hosted.Subject
	}
	return "local_" + credential.ID
}

func (s *Service) localAppProjects(ctx context.Context, credential hubapi.APICredential) ([]appBootstrapProject, error) {
	return s.hostedReadableProjectsPostgres(ctx, credential)
}

func (s *Service) localAppOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.localAppCredential(c)
		if err != nil {
			return s.localAppUnauthorized(c, status)
		}
		if org := c.Param("organization"); org != "" && org != s.localOrganization {
			return s.nativeAPIError(c, hubapi.NativeNotFound())
		}
		if !hubscope.HostedReadRequest(c) {
			ctx := c.Request().Context()
			scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.localOrganization), Credential: credential, RequireHostedAdmin: true}
			err := s.accessWrite(ctx, scope.Organization, func(w tenantstore.Writer) error {
				if err := w.LockAccessOrganization(ctx); err != nil {
					return err
				}
				if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
					return err
				}
				if _, err := w.AccessMember(ctx, appSubject(credential)); !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				now := s.config.Clock()
				return w.CreateAccessMember(ctx, accessstore.Member{OrganizationID: scope.Organization, UserID: appSubject(credential), Email: credential.Name, MembershipID: "local_" + credential.ID, Role: "admin", Active: true, PrincipalID: credential.ID, NotifyIssues: true, CreatedAt: now, UpdatedAt: now})
			})
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
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if !input.GrantAccess {
		return s.nativeAPIError(c, hubapi.NativeInvalid("Explicitly grant yourself project access"))
	}
	credential, status, err := s.localAppCredential(c)
	if err != nil {
		return s.localAppUnauthorized(c, status)
	}
	if c.Param("organization") != s.localOrganization {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.localOrganization), Credential: credential}
	c.Set("native_scope", scope)
	request := createNativeProjectRequest{Mutation: input.Mutation, Name: input.Name, States: HostedProjectStates()}
	return s.nativeMutation(c, request.Mutation, request, nil)
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

func (s *Service) appAdministrator(c echo.Context) (hubapi.APICredential, error) {
	if s.config.Hosted == nil {
		credential, _, err := s.localAppCredential(c)
		return credential, err
	}
	return s.hostedAdministrator(c)
}
