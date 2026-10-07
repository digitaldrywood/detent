package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type monthlyBudgetRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision     `json:"expected_revision,string"`
	Policy           budget.MonthlyPolicy `json:"policy"`
}

func (s *Service) getMonthlyBudget(c echo.Context) error {
	result, err := readMonthlyBudgetSettings(c.Request().Context(), s.database.db, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, result)
}

func (s *Service) updateMonthlyBudget(c echo.Context) error {
	var request monthlyBudgetRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "Monthly budgets require owner or admin access"})
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	result, err := s.executeNativeMutation(c.Request().Context(), scope, nativeCommandOptions{OperationID: "PUT " + c.Request().URL.EscapedPath(), Feature: "collaboration"}, request.Mutation, request, updateMonthlyBudgetOperation(request))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	return c.JSONBlob(http.StatusOK, result)
}

func updateMonthlyBudgetOperation(request monthlyBudgetRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if err := request.Policy.Validate(); err != nil {
			return nil, nativeInvalid(err.Error())
		}
		current, err := readMonthlyBudgetSettings(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		raw, err := json.Marshal(request.Policy)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO monthly_budget_policies(organization_id,project_id,revision,policy_json,actor_id,updated_at) VALUES(?,?,?,?,?,?)`, scope.organization, scope.project, current.Revision+1, string(raw), scope.credential.ID, formatHubTime(now))
		if err != nil {
			return nil, err
		}
		return readMonthlyBudgetSettings(ctx, tx, scope)
	}
}

func (s *Service) organizationMonthlyBudgetScope(c echo.Context) error {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	c.Set("native_scope", scope)
	if c.Request().Method == http.MethodGet {
		return s.getMonthlyBudget(c)
	}
	return s.updateMonthlyBudget(c)
}

func (s *Service) instanceMonthlyBudgetScope(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
	c.Set("native_scope", scope)
	if c.Request().Method == http.MethodGet {
		return s.getMonthlyBudget(c)
	}
	return s.updateMonthlyBudget(c)
}
