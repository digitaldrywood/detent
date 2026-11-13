package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerOnboardingRoutes(e *echo.Echo) {
	s.registerRunnerRepositoryRoutes(e)
	e.GET(nativeBase+"/onboarding/issue-intake", s.getGitHubBatch, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	e.POST(nativeBase+"/onboarding/issue-intake", s.commandGitHubBatch, s.requireOnboardingAdmin())
	e.POST(nativeBase+"/onboarding/issue-intake/result", s.reportGitHubBatch, s.requireNativeScope(apiScopeWorker))
	write := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	e.PUT(nativeBase+"/onboarding/integration", s.updateProjectIntegration, s.requireOnboardingAdmin())
	e.POST(nativeBase+"/onboarding/repository", s.bindNativeRepository, s.requireOnboardingAdmin())
	e.GET(nativeBase+"/onboarding", s.getOnboardingPage, s.requireAPIScope(apiScopeOperator, apiScopeWorker, apiScopeAdmin))
	e.PUT(nativeBase+"/onboarding", s.saveOnboarding, write)
	e.PUT(nativeBase+"/onboarding/policy", s.approveProjectPolicy, s.requireOnboardingAdmin())
	e.PUT(nativeBase+"/onboarding/artifact-services/:service", s.bindArtifactService, s.requireOnboardingAdmin())
}

func (s *Service) projectOnboarding(ctx context.Context, scope nativeScope) (onboarding.Project, error) {
	if scope.credential.Scope == apiScopeWorker {
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
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := request.Progress.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	return s.saveOnboardingProgressRecord(c, request)
}

func (s *Service) requireOnboardingAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireNativeScope(apiScopeOperator, apiScopeAdmin)(func(c echo.Context) error {
			credential := nativeRequestScope(c).credential
			if credential.Hosted != nil {
				if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
					return s.nativeAPIError(c, nativeNotFound())
				}
			} else if credential.Scope != apiScopeAdmin {
				return s.nativeAPIError(c, nativeNotFound())
			}
			return next(c)
		})
	}
}
