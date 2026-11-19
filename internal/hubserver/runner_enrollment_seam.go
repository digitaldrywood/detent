package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerenroll"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const runnerBase = "/api/v2/organizations/:organization/runners"
const enrollmentBase = "/api/v2/organizations/:organization/runner-enrollments"

func (s *Service) registerRunnerRoutes(e *echo.Echo) {
	s.registerRunnerUpdateRoutes(e)
	s.registerRunnerEnrollmentRoutes(e)
	s.registerRunnerAdviceRoutes(e)
	admin := s.requireInstanceAdmin()
	worker := s.requireAPIScope(hubapi.APIScopeWorker)
	e.GET(runnerBase+"/:runner", s.getRunnerIdentity, worker)
	e.POST(runnerBase+"/:runner/renew", s.renewRunnerIdentity, worker)
	e.POST(runnerBase+"/:runner/rotate", s.rotateRunnerIdentity, worker)
	e.DELETE(runnerBase+"/:runner", s.revokeRunnerIdentity, admin)
	e.GET(runnerBase, s.listRunnerRouting, admin)
	e.GET(runnerBase+"/:runner/routing", s.getRunnerRouting, s.requireAPIScope(hubapi.APIScopeWorker, hubapi.APIScopeAdmin))
	e.PUT(runnerBase+"/:runner/routing", s.updateRunnerRouting, admin)
	e.PUT(runnerBase+"/:runner/capacity", s.updateRunnerCapacity, admin)
	e.PUT("/api/v2/organizations/:organization/machines/:machine/routing", s.updateRunnerHost, admin)
	e.POST(hubapi.NativeBase+"/leases/:lease/validate", s.validateRunnerLease, s.requireNativeScope(hubapi.APIScopeWorker))
	e.POST(hubapi.NativeBase+"/machines/:machine/heartbeat", s.heartbeatNativeMachine, s.requireNativeScope(hubapi.APIScopeWorker))
}

func (s *Service) runnerEnrollmentService() *runnerenroll.Service {
	return runnerenroll.New(runnerenroll.Deps{
		Repository: s.config.TenantRepository, Now: s.config.Clock, NewID: hubapi.NewID,
		GenerateToken: s.config.Deps.GenerateToken, TokenFingerprint: hubapi.TokenFingerprint, WorkerScope: string(hubapi.APIScopeWorker),
		ApplyUsage: s.applyRunnerTenantUsage, CheckAdmission: s.checkRunnerAdmission,
		ValidPlatform: validRunnerPlatform, ValidSpriteSlug: validSpritesSlug,
	})
}

func (s *Service) createRunnerEnrollmentCommand(ctx context.Context, scope hubscope.NativeScope, request runnerauth.EnrollmentRequest) (any, error) {
	scope, err := s.runnerEnrollmentScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	value, err := s.runnerEnrollmentService().Create(ctx, s.authorization().Scope(scope), request)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) revokeRunnerEnrollmentCommand(ctx context.Context, scope hubscope.NativeScope, resource string) (any, error) {
	scope, err := s.runnerEnrollmentScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	value, err := s.runnerEnrollmentService().Revoke(ctx, s.authorization().Scope(scope), resource)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) runnerEnrollmentScope(ctx context.Context, scope hubscope.NativeScope) (hubscope.NativeScope, error) {
	scope.RequireHostedAdmin = scope.Credential.Hosted == nil
	if scope.Credential.Hosted == nil {
		return scope, nil
	}
	if err := s.requireHostedRunnerAdministration(ctx, scope.Credential); err != nil {
		return scope, err
	}
	scope.Credential.ManageRunners = true
	return scope, nil
}

func (s *Service) registerRunnerEnrollmentRoutes(e *echo.Echo) {
	runnerenroll.RegisterRoutes(e.Group(""), runnerenroll.HTTPDeps{
		Service: s.runnerEnrollmentService, Auth: s.requestAuth,
		Credential: func(c echo.Context) (hubauth.Credential, bool) {
			credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			return hubscope.AuthCredential(credential), ok
		},
		Admin:   s.requireInstanceAdmin(),
		Error:   func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Invalid: s.invalidAPIRequest,
	})
}
