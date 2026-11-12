package hubserver

import (
	"context"
	"net/http"

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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	var result monthlyBudgetSettings
	var err error

	result, err = readBillingMonthlySettings(ctx, s.database.billingRepository, scope)

	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, result)
}

func (s *Service) updateMonthlyBudget(c echo.Context) error {
	var request monthlyBudgetRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Monthly budgets require owner or admin access", apiErrorResponse{Code: "forbidden", Message: "Monthly budgets require owner or admin access"})
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	if s.database == nil || s.database.billingRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	return s.updateMonthlyBudgetRecord(c, scope, request)
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
