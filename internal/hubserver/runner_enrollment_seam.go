package hubserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
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
	worker := s.requireAPIScope(apiScopeWorker)
	e.GET(runnerBase+"/:runner", s.getRunnerIdentity, worker)
	e.POST(runnerBase+"/:runner/renew", s.renewRunnerIdentity, worker)
	e.POST(runnerBase+"/:runner/rotate", s.rotateRunnerIdentity, worker)
	e.DELETE(runnerBase+"/:runner", s.revokeRunnerIdentity, admin)
	e.GET(runnerBase, s.listRunnerRouting, admin)
	e.GET(runnerBase+"/:runner/routing", s.getRunnerRouting, s.requireAPIScope(apiScopeWorker, apiScopeAdmin))
	e.PUT(runnerBase+"/:runner/routing", s.updateRunnerRouting, admin)
	e.PUT(runnerBase+"/:runner/capacity", s.updateRunnerCapacity, admin)
	e.PUT("/api/v2/organizations/:organization/machines/:machine/routing", s.updateRunnerHost, admin)
	e.POST(nativeBase+"/leases/:lease/validate", s.validateRunnerLease, s.requireNativeScope(apiScopeWorker))
	e.POST(nativeBase+"/machines/:machine/heartbeat", s.heartbeatNativeMachine, s.requireNativeScope(apiScopeWorker))
}

func runnerTimeValid(now time.Time, created, expiry string) bool {
	start, err := parseTimeValue(created)
	if err != nil {
		return false
	}
	end, err := parseTimeValue(expiry)
	return err == nil && !now.Before(start) && now.Before(end)
}

func requireRunnerUpdate(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if result == nil {
		return nativeNotFound()
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) runnerEnrollmentService() *runnerenroll.Service {
	return runnerenroll.New(runnerenroll.Deps{
		Repository: s.config.TenantRepository, Now: s.config.now, NewID: newNativeID,
		GenerateToken: s.config.generateToken, TokenFingerprint: tokenFingerprint, WorkerScope: string(apiScopeWorker),
		ApplyUsage: s.applyRunnerTenantUsage, CheckAdmission: s.checkRunnerAdmission,
		ValidPlatform: validRunnerPlatform, ValidSpriteSlug: validSpritesSlug,
	})
}

func runnerUnauthorized() error { return fromFeatureError(runnerenroll.Unauthorized()) }
func (s *Service) createRunnerEnrollmentCommand(ctx context.Context, scope nativeScope, request runnerauth.EnrollmentRequest) (any, error) {
	scope, err := s.runnerEnrollmentScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	value, err := s.runnerEnrollmentService().Create(ctx, s.featureScope(scope), request)
	return value, fromFeatureError(err)
}

func (s *Service) revokeRunnerEnrollmentCommand(ctx context.Context, scope nativeScope, resource string) (any, error) {
	scope, err := s.runnerEnrollmentScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	value, err := s.runnerEnrollmentService().Revoke(ctx, s.featureScope(scope), resource)
	return value, fromFeatureError(err)
}

func (s *Service) runnerEnrollmentScope(ctx context.Context, scope nativeScope) (nativeScope, error) {
	scope.requireHostedAdmin = scope.credential.Hosted == nil
	if scope.credential.Hosted == nil {
		return scope, nil
	}
	if err := s.requireHostedRunnerAdministration(ctx, scope.credential); err != nil {
		return scope, err
	}
	scope.credential.ManageRunners = true
	return scope, nil
}

func (s *Service) registerRunnerEnrollmentRoutes(e *echo.Echo) {
	runnerenroll.RegisterRoutes(e.Group(""), runnerenroll.HTTPDeps{
		Service: s.runnerEnrollmentService, Auth: s.requestAuth,
		Credential: func(c echo.Context) (hubauth.Credential, bool) {
			credential, ok := c.Get("hub_api_credential").(apiCredential)
			return authCredential(credential), ok
		},
		Admin:   s.requireInstanceAdmin(),
		Error:   func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
		Invalid: s.invalidAPIRequest,
	})
}
