package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerrepos"
)

func (s *Service) runnerRepositoriesService() *runnerrepos.Service {
	db := s.database
	return runnerrepos.New(runnerrepos.Deps{
		Repository: s.config.TenantRepository, Now: s.config.Clock, Details: s.runDetailStore, Auth: s.requestAuth,
		RequireFeature: func(ctx context.Context, r billingstore.Reader, feature string, at time.Time) error {
			return hubapi.FromFeatureError(db.plansService().RequireFeatureRecord(ctx, r, feature, at))
		}, ApplyUsage: s.applyRunnerTenantUsage, RecordError: hubapi.ProjectRecordError,
	})
}

func (s *Service) registerRunnerRepositoryRoutes(e *echo.Echo) {
	runnerrepos.RegisterRoutes(e.Group(""), runnerrepos.HTTPDeps{
		Service: s.runnerRepositoriesService, Admin: s.requireOnboardingAdmin(), Worker: s.requireNativeScope(hubapi.APIScopeWorker),
		Input:   func(c echo.Context) hubauth.Input { return s.authInput(hubscope.NativeRequestScope(c)) },
		Feature: hostedMutationFeature, Decode: hubapi.DecodeAPIJSON,
		Error:   func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Invalid: s.invalidAPIRequest,
	})
}

func runnerRepositoryReportRoute(path string) bool { return runnerrepos.ReportRoute(path) }

func (s *Service) authorizeRunnerRepositoryProject(ctx context.Context, scope hubscope.NativeScope) error {
	return hubapi.FromFeatureError(s.runnerRepositoriesService().AuthorizeProject(ctx, s.authInput(scope)))
}
