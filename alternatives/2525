package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerOnboardingRoutes(e *echo.Echo) {
	s.registerRunnerRepositoryRoutes(e)
	e.GET(hubapi.NativeBase+"/onboarding/issue-intake", s.getGitHubBatch, s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	e.POST(hubapi.NativeBase+"/onboarding/issue-intake", s.commandGitHubBatch, s.requireOnboardingAdmin())
	e.POST(hubapi.NativeBase+"/onboarding/issue-intake/result", s.reportGitHubBatch, s.requireNativeScope(hubapi.APIScopeWorker))
	write := s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin)
	e.PUT(hubapi.NativeBase+"/onboarding/integration", s.updateProjectIntegration, s.requireOnboardingAdmin())
	e.POST(hubapi.NativeBase+"/onboarding/repository", s.bindNativeRepository, s.requireOnboardingAdmin())
	e.GET(hubapi.NativeBase+"/onboarding", s.getOnboardingPage, s.requireAPIScope(hubapi.APIScopeOperator, hubapi.APIScopeWorker, hubapi.APIScopeAdmin))
	e.PUT(hubapi.NativeBase+"/onboarding", s.saveOnboarding, write)
	e.PUT(hubapi.NativeBase+"/onboarding/policy", s.approveProjectPolicy, s.requireOnboardingAdmin())
	e.PUT(hubapi.NativeBase+"/onboarding/artifact-services/:service", s.bindArtifactService, s.requireOnboardingAdmin())
}

func (s *Service) onboardingAdministrationDenial(ctx context.Context, scope hubscope.NativeScope) error {
	if s.config.TenantRepository == nil {
		return hubapi.NativeNotFound()
	}
	scope.RequireHostedAdmin = true
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAccessScope(ctx, r, scope, true)
	})
	if err == nil {
		return accessstore.ErrDenied
	}
	return err
}

func (s *Service) projectOnboarding(ctx context.Context, scope hubscope.NativeScope) (onboarding.Project, error) {
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		progress, err := s.readOnboardingProgressRecord(ctx, scope)
		return onboarding.Project{Progress: progress}, err
	}
	return s.projectOnboardingRecord(ctx, scope)
}

type saveOnboardingRequest struct {
	tracker.Mutation
	Progress onboarding.Progress `json:"progress"`
}

func (s *Service) saveOnboarding(c echo.Context) error {
	var request saveOnboardingRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := request.Progress.Validate(); err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	return s.saveOnboardingProgressRecord(c, request)
}

func (s *Service) requireOnboardingAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin)(func(c echo.Context) error {
			scope := hubscope.NativeRequestScope(c)
			credential := scope.Credential
			if credential.Hosted != nil && (credential.HostedRole == "owner" || credential.HostedRole == "admin") || credential.Hosted == nil && credential.Scope == hubapi.APIScopeAdmin {
				return next(c)
			}
			return s.nativeAPIError(c, s.onboardingAdministrationDenial(c.Request().Context(), scope))
		})
	}
}
