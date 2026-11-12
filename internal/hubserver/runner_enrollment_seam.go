package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerenroll"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerBase = "/api/v2/organizations/:organization/runners"
const enrollmentBase = "/api/v2/organizations/:organization/runner-enrollments"

func (s *Service) registerRunnerRoutes(e *echo.Echo) {
	admin := s.requireInstanceAdmin()
	worker := s.requireAPIScope(apiScopeWorker)
	e.POST(enrollmentBase, s.createRunnerEnrollment, admin)
	e.DELETE(enrollmentBase+"/:enrollment", s.revokeRunnerEnrollment, admin)
	e.POST(enrollmentBase+"/redeem", s.redeemRunnerEnrollment)
	e.GET(runnerBase+"/:runner", s.getRunnerIdentity, worker)
	e.GET(runnerBase+"/:runner/update/receipt", s.getRunnerUpdateReceipt, worker)
	e.PUT(runnerBase+"/:runner/update/receipt", s.putRunnerUpdateReceipt, worker)
	e.POST(runnerBase+"/:runner/renew", s.renewRunnerIdentity, worker)
	e.POST(runnerBase+"/:runner/rotate", s.rotateRunnerIdentity, worker)
	e.DELETE(runnerBase+"/:runner", s.revokeRunnerIdentity, admin)
	e.GET(runnerBase, s.listRunnerRouting, admin)
	e.GET(runnerBase+"/:runner/routing", s.getRunnerRouting, s.requireAPIScope(apiScopeWorker, apiScopeAdmin))
	e.PUT(runnerBase+"/:runner/routing", s.updateRunnerRouting, admin)
	e.GET(runnerBase+"/:runner/capacity", s.getRunnerCapacity, admin)
	e.GET(runnerBase+"/:runner/update", s.getRunnerUpdate, admin)
	e.POST(runnerBase+"/:runner/update/apply", s.applyRunnerUpdate, admin)
	e.GET(urgentRunnerUpdatePath, s.getUrgentRunnerUpdate, admin)
	e.PUT(urgentRunnerUpdatePath, s.markUrgentRunnerUpdate, admin)
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

func (s *Service) createRunnerEnrollment(c echo.Context) error {
	var request runnerauth.EnrollmentRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	value, err := s.createRunnerEnrollmentCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, value)
}

func (s *Service) revokeRunnerEnrollment(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	_, err := s.revokeRunnerEnrollmentCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("enrollment"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.NoContent(http.StatusNoContent)
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

func (s *Service) redeemRunnerEnrollment(c echo.Context) error {
	token, err := apiBearerToken(c)
	if err != nil {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	var request runnerauth.Redemption
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	identity, err := s.runnerEnrollmentService().Redeem(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), token, request)
	if err != nil {
		return s.nativeAPIError(c, fromFeatureError(err))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, identity)
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
func runnerCollision() error    { return fromFeatureError(runnerenroll.Collision()) }

func (s *Service) createRunnerEnrollmentCommand(ctx context.Context, scope nativeScope, request runnerauth.EnrollmentRequest) (any, error) {
	scope.requireHostedAdmin = true
	value, err := s.runnerEnrollmentService().Create(ctx, s.featureScope(scope), request)
	return value, fromFeatureError(err)
}

func (s *Service) revokeRunnerEnrollmentCommand(ctx context.Context, scope nativeScope, resource string) (any, error) {
	scope.requireHostedAdmin = true
	value, err := s.runnerEnrollmentService().Revoke(ctx, s.featureScope(scope), resource)
	return value, fromFeatureError(err)
}

func (s *Service) runnerEnrollmentWriteTenant(ctx context.Context, scope nativeScope, write func(context.Context, tenantstore.Writer, time.Time) error) error {
	scope.requireHostedAdmin = true
	return fromFeatureError(s.runnerEnrollmentService().Write(ctx, s.featureScope(scope), write))
}

func (s *Service) redeemRunnerEnrollmentTenant(ctx context.Context, organization tracker.OrganizationID, token string, request runnerauth.Redemption) (runnerauth.Identity, error) {
	value, err := s.runnerEnrollmentService().RedeemTenant(ctx, organization, token, request)
	return value, fromFeatureError(err)
}
