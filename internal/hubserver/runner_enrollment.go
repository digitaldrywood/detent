package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
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

type runnerCommittedResponse func(context.Context, *sql.Tx, time.Time) (any, error)

func (s *Service) runnerTransaction(c echo.Context, status int, operation func(context.Context, *sql.Tx, time.Time) (any, error)) error {
	ctx := s.runDetailContext(c.Request().Context())
	c.SetRequest(c.Request().WithContext(ctx))
	contentMutation(ctx, true)
	defer contentMutation(ctx, false)
	_, err := prepareContentMutation(ctx, s.database.reader, func() (struct{}, error) {
		return struct{}{}, s.commitRunnerTransaction(c, status, operation)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if pending, ok := ctx.Value(pendingContentKey{}).(*pendingContentMutation); ok {
		if _, err := s.uploadPendingContent(ctx, pending.objects); err != nil {
			panic(err)
		}
	}
	return nil
}

func (s *Service) commitRunnerTransaction(c echo.Context, status int, operation func(context.Context, *sql.Tx, time.Time) (any, error)) error {
	ctx := c.Request().Context()
	metrics := hostedRunnerTransactionMetrics(c.Path())
	heartbeat := strings.HasSuffix(c.Path(), "/heartbeat")
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if pending, ok := ctx.Value(pendingContentKey{}).(*pendingContentMutation); ok {
		pending.tx, pending.objects = tx, nil
		defer func() { pending.tx = nil }()
	}
	step := func(string) {}
	if heartbeat {
		step = heartbeatWriterSteps(ctx)
	}
	defer step("")
	step("authority")
	now, err := s.database.currentTime()
	if err != nil {
		return err
	}
	if credential, ok := c.Get("hub_api_credential").(apiCredential); ok {
		if err := s.recheckHostedMutation(ctx, tx, &nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}); err != nil {
			return err
		}
		if err := requireCredentialAuthority(ctx, tx, credential, now); err != nil {
			return err
		}
	}
	step("consumption_before")
	before, err := s.database.hostedConsumption(ctx, tx, now, metrics...)
	if err != nil {
		return err
	}
	step("dispatch_before")
	var dispatchBefore string
	if heartbeat {
		dispatchBefore, err = runnerDispatchFingerprint(ctx, tx, nativeRequestScope(c), c.Param("machine"), now)
		if err != nil {
			return err
		}
	}
	step("")
	value, err := operation(ctx, tx, now)
	if err != nil {
		return err
	}
	step("dispatch_after")
	notify := !heartbeat && c.Request().Method != http.MethodGet && c.Path() != nativeBase+"/claims/preview" && !strings.HasSuffix(c.Path(), "/renew") && !strings.HasSuffix(c.Path(), "/validate")
	if heartbeat {
		after, err := runnerDispatchFingerprint(ctx, tx, nativeRequestScope(c), c.Param("machine"), now)
		if err != nil {
			return err
		}
		notify = dispatchBefore != after
	}
	step("consumption_after")
	completion := c.Request().Method == http.MethodDelete
	if s.database.plans() != nil && (heartbeat || strings.HasSuffix(c.Path(), "/machines/register")) {
		credential, ok := c.Get("hub_api_credential").(apiCredential)
		if !ok {
			return runnerUnauthorized()
		}
		active, err := countHostedAfter(ctx, tx, "SELECT l.expires_at FROM leases l JOIN lease_runners r ON r.lease_id=l.lease_id WHERE l.released_at IS NULL AND r.runner_id=?", now, credential.Runner.RunnerID)
		if err != nil {
			return err
		}
		completion = active > 0
	}
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		completion = completion || content.completion
		content.mu.Unlock()
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, completion, metrics...); err != nil {
		return err
	}
	step("usage")
	if heartbeat {
		if err := s.database.recordHostedUsage(ctx, tx, now, "heartbeats", 1); err != nil {
			return err
		}
	}
	step("commit")
	if err := tx.Commit(); err != nil {
		return err
	}
	step("")
	if notify {
		s.notifications.notify(dispatchNotificationKey(tracker.OrganizationID(c.Param("organization"))))
	}
	if heartbeat {
		s.startSpriteLifecycle(nativeRequestScope(c), "")
	}
	if response, ok := value.(runnerCommittedResponse); ok {
		value, err = s.readCommittedRunnerResponse(c, response)
		if err != nil {
			return err
		}
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	if status == http.StatusNoContent {
		return c.NoContent(status)
	}
	return c.JSON(status, value)
}

func (s *Service) readCommittedRunnerResponse(c echo.Context, response runnerCommittedResponse) (any, error) {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	ctx = s.freshRunDetailContext(ctx)
	for {
		value, err := s.readCommittedRunnerResponseOnce(c, ctx, response)
		var hydration *claimHydration
		if !errors.As(err, &hydration) {
			return value, err
		}
		if err := hydration.run(ctx, s.database.reader); err != nil {
			return nil, err
		}
	}
}

func (s *Service) readCommittedRunnerResponseOnce(c echo.Context, ctx context.Context, response runnerCommittedResponse) (any, error) {
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	scope := nativeRequestScope(c)
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return nil, err
	}
	if err := requireCredentialAuthority(ctx, tx, scope.credential, now); err != nil {
		return nil, err
	}
	return response(ctx, tx, now)
}

// runnerReadTransaction answers a runner request that only reads, from a
// consistent reader snapshot, so the answer never occupies the writer; only
// the plan's request accounting writes. Writes that depend on the answer
// re-check fencing themselves.
func (s *Service) runnerReadTransaction(c echo.Context, operation func(context.Context, *sql.Tx, time.Time) (any, error)) error {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	tx, err := s.database.reader.BeginTx(readCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if credential, ok := c.Get("hub_api_credential").(apiCredential); ok {
		if err := s.recheckHostedMutation(ctx, tx, &nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}); err != nil {
			return s.nativeAPIError(c, err)
		}
		if err := requireCredentialAuthority(ctx, tx, credential, now); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	value, err := operation(ctx, tx, now)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := tx.Rollback(); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.chargeRunnerRead(ctx, c.Path(), now); err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

// chargeRunnerRead records a successful runner read against the hosted plan's
// request allowance in one short write, refusing it when the window is spent.
func (s *Service) chargeRunnerRead(ctx context.Context, path string, now time.Time) error {
	if s.database.plans() == nil {
		return nil
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	metrics := hostedRunnerTransactionMetrics(path)
	before, err := s.database.hostedConsumption(ctx, tx, now, metrics...)
	if err != nil {
		return err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false, metrics...); err != nil {
		return err
	}
	return tx.Commit()
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
	if s.config.TenantRepository != nil {
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
	return s.runnerAdminTransaction(ctx, scope, false, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		return s.createRunnerEnrollmentInTx(ctx, tx, scope, request, now)
	})
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
	if s.config.TenantRepository != nil {
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
	return s.runnerAdminTransaction(ctx, scope, true, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		result, err := tx.ExecContext(ctx, "UPDATE runner_enrollments SET revoked_at = ? WHERE id = ? AND organization_id = ? AND redeemed_at IS NULL AND revoked_at IS NULL", formatHubTime(now), resource, string(scope.organization))
		return struct{}{}, requireRunnerUpdate(result, err)
	})
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
	if s.config.TenantRepository != nil {
		identity, err := s.redeemRunnerEnrollmentTenant(c.Request().Context(), tracker.OrganizationID(c.Param("organization")), token, request)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusCreated, identity)
	}
	return s.runnerTransaction(c, http.StatusCreated, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		var id, operations, created, expires, actor, enrollmentScope string
		var binding runnerauth.Binding
		var redeemed, revoked sql.NullString
		var shared bool
		err := tx.QueryRowContext(ctx, `SELECT id, runner_id, machine_id, operations_json, created_at, expires_at, created_by, redeemed_at, revoked_at, shared_machine, scope
FROM runner_enrollments WHERE token_hash = ? AND organization_id = ?`, apikey.HashToken(token), c.Param("organization")).Scan(&id, &binding.RunnerID, &binding.MachineID, &operations, &created, &expires, &actor, &redeemed, &revoked, &shared, &enrollmentScope)
		unbound := binding == runnerauth.Binding{}
		if err != nil || !unbound && binding != request.Binding || redeemed.Valid || revoked.Valid || !runnerTimeValid(now, created, expires) {
			return nil, runnerUnauthorized()
		}
		binding = request.Binding
		if err := runnerBindingAvailable(ctx, tx, binding, c.Param("organization"), shared); err != nil {
			return nil, err
		}
		identity := runnerauth.Identity{Scope: enrollmentScope, Binding: binding, OrganizationID: tracker.OrganizationID(c.Param("organization")), ExpiresAt: now.Add(runnerauth.CredentialTTL)}
		if err := json.Unmarshal([]byte(operations), &identity.Operations); err != nil {
			return nil, err
		}
		hash := apikey.HashToken(request.Credential)
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at, native_only, expires_at)
VALUES (?, ?, ?, ?, 'worker', ?, ?, 1, ?)`, binding.RunnerID, binding.RunnerID, hash, tokenFingerprint(hash), formatHubTime(now), formatHubTime(now), formatHubTime(identity.ExpiresAt)); err != nil {
			return nil, runnerCollision()
		}
		if !shared {
			if _, err := tx.ExecContext(ctx, `INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at, organization_id, token_id,machine_facts_ready)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,1)`, binding.MachineID, request.Hostname, request.DisplayName, request.Capacity, request.Version, formatHubTime(now), formatHubTime(now), formatHubTime(now), identity.OrganizationID, binding.RunnerID); err != nil {
				return nil, err
			}
		}
		if request.SpriteName != "" && !shared {
			if err := updateMachineSpriteFact(ctx, tx, binding.MachineID, "sprite_name", request.SpriteName); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runner_identities (id, organization_id, machine_id, token_id, enrollment_id, operations_json, created_at, display_name, capacity_limit, reported_capacity, os, architecture, last_heartbeat_at, scope, operations_known) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`, binding.RunnerID, identity.OrganizationID, binding.MachineID, binding.RunnerID, id, operations, formatHubTime(now), request.DisplayName, request.Capacity, request.Capacity, request.OS, request.Architecture, formatHubTime(now), enrollmentScope); err != nil {
			return nil, err
		}
		if err := writeRunnerOperations(ctx, tx, binding.RunnerID, operations); err != nil {
			return nil, err
		}
		if request.SpriteName != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE runner_identities SET tags_json='["sprite"]' WHERE id=? AND EXISTS (SELECT 1 FROM organization_sprite_members m WHERE m.enrollment_id=? AND m.organization_id=? AND m.name=?)`, binding.RunnerID, id, identity.OrganizationID, request.SpriteName); err != nil {
				return nil, err
			}
		}
		settings, err := marshalNative(runnerSettings{IsolationTier: request.IsolationTier})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET routing_settings_json = ? WHERE id = ?", settings, binding.RunnerID); err != nil {
			return nil, err
		}
		if err := storeRunnerRoutingFacts(ctx, tx, binding.RunnerID, settings); err != nil {
			return nil, err
		}
		if err := updateRunnerIsolationReport(ctx, tx, nativeScope{organization: identity.OrganizationID, credential: apiCredential{ID: binding.RunnerID, Runner: identity}}, request.BackendIsolation); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO token_grants (token_id, organization_id, project_id) SELECT ?, organization_id, project_id FROM runner_enrollment_projects WHERE enrollment_id = ?`, binding.RunnerID, id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE runner_enrollments SET redeemed_at = ?, runner_id = ?, machine_id = ? WHERE id = ?", formatHubTime(now), binding.RunnerID, binding.MachineID, id); err != nil {
			return nil, err
		}
		if err := recordRunnerEvent(ctx, tx, binding.RunnerID, actor, "enrolled", now); err != nil {
			return nil, err
		}
		identity.ProjectIDs, err = readRunnerProjects(ctx, tx, binding.RunnerID)
		return identity, err
	})
}
