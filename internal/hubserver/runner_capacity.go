package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerCapacityChange struct {
	runnerauth.CapacityRequest
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

func freshCapacityConfig(r runnerauth.Runner, now time.Time) bool {
	return r.CapacityConfig != nil && !now.Before(r.CapacityConfig.ObservedAt) && now.Before(r.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout)) && !now.Before(r.LastHeartbeatAt) && now.Before(r.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout))
}

func (s *Service) readRunnerCapacity(ctx context.Context, scope nativeScope, resource, backend string) (any, error) {
	if backend != "" && (runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("0", 64), Capacity: 1, Backend: backend}).Validate() != nil {
		return nil, nativeInvalid("Backend must be a bounded provider identifier")
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := s.requireRunnerAdministration(ctx, tx, scope, now); err != nil {
		return nil, err
	}
	r, err := readRunner(ctx, tx, scope.organization, resource, now)
	if err != nil {
		return nil, err
	}
	return s.runnerCapacityView(ctx, tx, r, backend, now)
}

func (s *Service) updateRunnerCapacityCommand(ctx context.Context, scope nativeScope, resource string, request runnerCapacityChange) (any, error) {
	if request.ExpectedRevision < 1 || request.CapacityRequest.Validate() != nil || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativeInvalid("A bounded capacity, configuration revision, runner revision and idempotency key are required")
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := s.requireRunnerAdministration(ctx, tx, scope, now); err != nil {
		return nil, err
	}

	r, err := readRunner(ctx, tx, scope.organization, resource, now)
	if err != nil {
		return nil, err
	}
	operation := "runner_capacity " + resource
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	raw, found, err := nativeCommandReceipt(ctx, tx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	}

	if r.Revision != request.ExpectedRevision {
		return nil, nativeConflict(tracker.Revision(r.Revision))
	}
	if !freshCapacityConfig(r, now) || r.CapacityConfig.Revision != request.ExpectedConfigRevision {
		return nil, &nativeError{Code: "revision_conflict", Message: "Current enrolled runner configuration evidence is required; read runner capacity again", status: http.StatusConflict}
	}
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	r.CapacityRequest = &request.CapacityRequest
	settings, err := marshalNative(settingsFromRouting(r.Routing))
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET capacity_limit = ?, routing_settings_json = ?, revision = revision + 1 WHERE organization_id = ? AND id = ?", request.Capacity, settings, scope.organization, resource); err != nil {
		return nil, err
	}
	r, err = readRunner(ctx, tx, scope.organization, resource, now)
	if err != nil {
		return nil, err
	}
	view, err := s.runnerCapacityView(ctx, tx, r, request.Backend, now)
	if err != nil {
		return nil, err
	}
	raw, err = marshalNative(view)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO native_commands (organization_id, actor_id, operation, command_key, request_hash, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)", scope.organization, scope.credential.ID, operation, request.IdempotencyKey, hash, raw, formatHubTime(now)); err != nil {
		return nil, err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return view, nil
}

func (s *Service) getRunnerCapacity(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	value, err := s.readRunnerCapacity(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"), c.QueryParam("backend"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateRunnerCapacity(c echo.Context) error {
	var request runnerCapacityChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	value, err := s.updateRunnerCapacityCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) runnerCapacityView(ctx context.Context, query nativeQueryer, r runnerauth.Runner, backend string, now time.Time) (runnerauth.CapacityView, error) {
	view := r.CapacityView(backend, now)
	if s.database.hostedPlans == nil {
		return view, nil
	}
	entitlement, err := s.database.hostedEntitlement(ctx, query, now)
	if err != nil {
		return view, err
	}
	if !slices.Contains(entitlement.Features, "native_execution") {
		zero := 0
		view.Limits = append(view.Limits, runnerauth.CapacityLimit{Owner: "plan", Limit: &zero, Constraint: "The current plan does not permit native execution.", ObservedAt: now})
		view.Effective = &zero
		view.Status = "partially_applied"
	}
	return view, nil
}
