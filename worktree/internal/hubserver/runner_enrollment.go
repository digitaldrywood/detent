package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
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

func runnerUnauthorized() error {
	return &nativeError{Code: "unauthorized", Message: "Runner credential or enrollment is invalid or inactive", status: http.StatusUnauthorized}
}

func runnerCollision() error {
	return &nativeError{Code: "identity_collision", Message: "Identity is already registered; create a fresh host identity and enroll it explicitly", status: http.StatusConflict, publicMessage: true}
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

func (s *Service) createRunnerEnrollmentCommand(ctx context.Context, scope nativeScope, request runnerauth.EnrollmentRequest) (any, error) {
	if !request.Valid() && !request.Unbound() || !runnerauth.ValidOperations(request.Operations) || request.Scope != "" && request.Scope != "projects" && request.Scope != "organization" || request.Scope == "organization" && len(request.ProjectIDs) > 0 || request.Scope != "organization" && len(request.ProjectIDs) == 0 || len(request.ProjectIDs) > 100 || request.TTLSeconds <= 0 || request.TTLSeconds > int64(runnerauth.MaxEnrollmentTTL/time.Second) {
		return nil, &nativeError{Code: "invalid_request", Message: "Enrollment requires valid or omitted host IDs, a project or organization scope and operations, and a TTL of 1 to 900 seconds", status: http.StatusUnprocessableEntity, publicMessage: true}
	}
	if request.Scope == "" {
		request.Scope = "projects"
	}
	token, err := s.config.generateToken()
	if err != nil {
		return nil, err
	}
	var enrollment runnerauth.Enrollment
	err = s.runnerEnrollmentWriteTenant(ctx, scope, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
		for i, project := range request.ProjectIDs {
			record, err := w.ProjectRecord(ctx, project)
			if err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nativePublicInvalid("Enrollment projects must be unique and belong to the organization")
				}
				return err
			}
			if record.Core.DeletedAt != nil || slices.Contains(request.ProjectIDs[:i], project) {
				return nativePublicInvalid("Enrollment projects must be unique and belong to the organization")
			}
		}
		if !request.Unbound() {
			if err := w.RunnerBindingAvailable(ctx, request.RunnerID, request.MachineID, request.SharedMachine); err != nil {
				if errors.Is(err, runnerstore.ErrIdentityCollision) {
					return runnerCollision()
				}
				return err
			}
		}
		enrollment = runnerauth.Enrollment{ID: newNativeID("enrollment"), Token: token, ExpiresAt: now.Add(time.Duration(request.TTLSeconds) * time.Second)}
		if err := w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: scope.organization, ID: enrollment.ID, RunnerID: request.RunnerID, MachineID: request.MachineID, TokenHash: apikey.HashToken(token), Scope: request.Scope, SharedMachine: request.SharedMachine, Operations: slices.Clone(request.Operations), ProjectIDs: slices.Clone(request.ProjectIDs), CreatedAt: now, ExpiresAt: enrollment.ExpiresAt, CreatedBy: scope.credential.ID}); err != nil {
			return err
		}
		return s.applyRunnerTenantUsage(ctx, w, now, enrollment.ID, false, false)
	})
	if err != nil {
		return nil, err
	}
	return enrollment, nil
}

func (s *Service) createRunnerEnrollmentInTx(ctx context.Context, tx *sql.Tx, scope nativeScope, request runnerauth.EnrollmentRequest, now time.Time) (runnerauth.Enrollment, error) {
	if request.Scope == "" {
		request.Scope = "projects"
	}
	for i, project := range request.ProjectIDs {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE deleted_at IS NULL AND organization_id = ? AND id = ?", string(scope.organization), project).Scan(&count); err != nil {
			return runnerauth.Enrollment{}, err
		}
		if count != 1 || slices.Contains(request.ProjectIDs[:i], project) {
			return runnerauth.Enrollment{}, &nativeError{Code: "invalid_request", Message: "Enrollment projects must be unique and belong to the organization", status: http.StatusUnprocessableEntity, publicMessage: true}
		}
	}
	if !request.Unbound() {
		if err := runnerBindingAvailable(ctx, tx, request.Binding, string(scope.organization), request.SharedMachine); err != nil {
			return runnerauth.Enrollment{}, err
		}
	}
	token, err := s.config.generateToken()
	if err != nil {
		return runnerauth.Enrollment{}, err
	}
	result := runnerauth.Enrollment{ID: newNativeID("enrollment"), Token: token, ExpiresAt: now.Add(time.Duration(request.TTLSeconds) * time.Second)}
	operations, err := marshalNative(request.Operations)
	if err != nil {
		return runnerauth.Enrollment{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runner_enrollments (id, organization_id, runner_id, machine_id, token_hash, operations_json, created_at, expires_at, created_by, shared_machine, scope)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, result.ID, string(scope.organization), request.RunnerID, request.MachineID, apikey.HashToken(token), operations, formatHubTime(now), formatHubTime(result.ExpiresAt), scope.credential.ID, request.SharedMachine, request.Scope); err != nil {
		return runnerauth.Enrollment{}, err
	}
	for _, project := range request.ProjectIDs {
		if _, err := tx.ExecContext(ctx, "INSERT INTO runner_enrollment_projects (enrollment_id, organization_id, project_id) VALUES (?, ?, ?)", result.ID, string(scope.organization), project); err != nil {
			return runnerauth.Enrollment{}, err
		}
	}
	return result, nil
}

func runnerIDsAvailable(ctx context.Context, tx *sql.Tx, binding runnerauth.Binding) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM machines WHERE id = ?) + (SELECT count(*) FROM runner_identities WHERE id = ?)`, binding.MachineID, binding.RunnerID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return runnerCollision()
	}
	return nil
}

func runnerBindingAvailable(ctx context.Context, tx *sql.Tx, binding runnerauth.Binding, organization string, shared bool) error {
	if !shared {
		return runnerIDsAvailable(ctx, tx, binding)
	}
	var hosts, runners int
	if err := tx.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM machines m WHERE m.id = ? AND m.organization_id = ? AND EXISTS (SELECT 1 FROM runner_identities r WHERE r.machine_id = m.id)),
(SELECT count(*) FROM runner_identities WHERE id = ?)`, binding.MachineID, organization, binding.RunnerID).Scan(&hosts, &runners); err != nil {
		return err
	}
	if hosts != 1 || runners != 0 {
		return runnerCollision()
	}
	return nil
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

func (s *Service) revokeRunnerEnrollmentCommand(ctx context.Context, scope nativeScope, resource string) (any, error) {
	if err := s.runnerEnrollmentWriteTenant(ctx, scope, func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
		if err := w.RevokeRunnerEnrollment(ctx, resource, now); errors.Is(err, corestore.ErrRevisionConflict) {
			return nativeNotFound()
		} else {
			if err != nil {
				return err
			}
		}
		return s.applyRunnerTenantUsage(ctx, w, now, resource, true, false)
	}); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (s *Service) runnerEnrollmentWriteTenant(ctx context.Context, scope nativeScope, write func(context.Context, tenantstore.Writer, time.Time) error) error {
	if scope.organization == "" {
		return nativeNotFound()
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	scope.requireHostedAdmin = true
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		return write(ctx, w, s.config.now().UTC())
	})
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
	if !request.Valid() || !runnerauth.ValidCredential(request.Credential) || token == request.Credential || strings.TrimSpace(request.Hostname) == "" || len(request.Hostname) > 200 || len(request.DisplayName) > 200 || request.Capacity < 0 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || !validRunnerPlatform(request.OS, request.Architecture) {
		return s.nativeAPIError(c, nativePublicInvalid("Host identity, a separate generated credential, hostname, version and nonnegative capacity are required"))
	}
	if request.SpriteName != "" && (request.SpriteName != request.Hostname || !validSpritesSlug(request.SpriteName)) {
		return s.nativeAPIError(c, nativePublicInvalid("Sprite name must match the runner hostname"))
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if request.IsolationTier == "" {
		request.IsolationTier = isolation.Sandbox
	}
	if request.IsolationTier != isolation.Sandbox && request.IsolationTier != isolation.NativeTrusted {
		return s.nativeAPIError(c, nativePublicInvalid("Isolation tier must be sandbox or native-trusted"))
	}
	identity, err := s.redeemRunnerEnrollmentTenant(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), token, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, identity)
}
