package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerrepos"
)

func (s *Service) runnerRepositoriesService() *runnerrepos.Service {
	return runnerrepos.New(runnerrepos.Deps{
		Repository: s.config.TenantRepository, Now: s.config.now, Details: s.runDetailStore, Auth: s.requestAuth,
		RequireFeature: s.database.requireHostedFeatureRecord, ApplyUsage: s.applyRunnerTenantUsage, RecordError: projectRecordError,
	})
}

func (s *Service) registerRunnerRepositoryRoutes(e *echo.Echo) {
	runnerrepos.RegisterRoutes(e.Group(""), runnerrepos.HTTPDeps{
		Service: s.runnerRepositoriesService, Admin: s.requireOnboardingAdmin(), Worker: s.requireNativeScope(apiScopeWorker),
		Input:   func(c echo.Context) hubauth.Input { return s.authInput(nativeRequestScope(c)) },
		Feature: hostedMutationFeature, Decode: decodeAPIJSON,
		Error:   func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
		Invalid: s.invalidAPIRequest,
	})
}

func runnerRepositoryReportRoute(path string) bool { return runnerrepos.ReportRoute(path) }

func (s *Service) authorizeRunnerRepositoryProject(ctx context.Context, scope nativeScope) error {
	return fromFeatureError(s.runnerRepositoriesService().AuthorizeProject(ctx, s.authInput(scope)))
}
