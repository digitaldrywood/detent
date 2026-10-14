package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type spriteUsageRequest struct {
	tracker.Mutation
	Observations []costObservation `json:"observations"`
}

func spriteCostMetrics() []string {
	return []string{"cpu", "ram", "storage_hot", "storage_cold"}
}

func validateSpriteCostObservation(o *costObservation, now time.Time) error {
	if o.Provider != "fly_sprites" || o.Bucket != spriteCostBucket {
		return nativeInvalid("Sprite usage requires the fly_sprites provider and sprite_infrastructure bucket")
	}
	unit := "GB-hour"
	if o.Metric == "cpu" {
		unit = "CPU-hour"
	}
	if !slices.Contains(spriteCostMetrics(), o.Metric) || o.Unit != unit {
		return nativeInvalid("Sprite quantities use cpu in CPU-hour or ram, storage_hot, storage_cold in GB-hour")
	}
	return normalizeCostObservation(o, now)
}

func (s *Service) registerCostUsageRoutes(e *echo.Echo) {
	if s.config.Hosted == nil {
		e.GET("/api/v2/organizations/:organization/monthly-budget", s.instanceMonthlyBudgetScope, s.requireInstanceAdmin())
		e.PUT("/api/v2/organizations/:organization/monthly-budget", s.instanceMonthlyBudgetScope, s.requireInstanceAdmin())
	}
	e.GET(nativeBase+"/monthly-budget", s.getMonthlyBudget, s.requireNativeScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin))
	e.PUT(nativeBase+"/monthly-budget", s.updateMonthlyBudget, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	e.GET(nativeBase+"/usage/monthly", s.projectMonthlyCosts, s.requireNativeScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin))
	e.GET("/api/v2/organizations/:organization/usage/monthly", s.organizationMonthlyCosts, s.requireInstanceAdmin())
	e.POST(nativeBase+"/usage/sprites", s.ingestSpriteUsage, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
}

func (s *Service) projectMonthlyCosts(c echo.Context) error {
	scope := nativeRequestScope(c)
	window, err := costMonth(c.QueryParam("month"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.database.monthlyCosts(c.Request().Context(), string(scope.organization), "project", []string{string(scope.project)}, window, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) organizationMonthlyCosts(c echo.Context) error {
	organization := c.Param("organization")
	if s.config.Hosted != nil && organization != s.config.Hosted.OrganizationID {
		return s.nativeAPIError(c, nativeNotFound())
	}
	var exists int
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM organizations WHERE id=?", organization).Scan(&exists); err != nil {
		return s.nativeAPIError(c, err)
	}
	if exists != 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	window, err := costMonth(c.QueryParam("month"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.database.monthlyCosts(c.Request().Context(), organization, "organization", nil, window, s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, report)
}

func (s *Service) ingestSpriteUsage(c echo.Context) error {
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "Importing infrastructure usage requires owner or admin access"})
	}
	scope.requireHostedAdmin = true
	c.Set("native_scope", scope)
	var request spriteUsageRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.nativeAPIError(c, nativeInvalid("Enter sanitized Sprite usage observations"))
	}
	result, err := s.importSpriteUsageCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) importSpriteUsageCommand(ctx context.Context, scope nativeScope, request spriteUsageRequest) ([]byte, error) {
	if !canManageProjectSecrets(scope.credential) {
		return nil, &nativeError{Code: "forbidden", Message: "Importing infrastructure usage requires owner or admin access", status: http.StatusForbidden}
	}
	scope.requireHostedAdmin = true
	if err := validateSpriteUsageRequest(&request, s.config.now()); err != nil {
		return nil, err
	}
	return s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: "sprite_usage.import", Feature: "collaboration"}, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		recorded := 0
		for _, observation := range request.Observations {
			changed, err := recordCostObservation(ctx, tx, string(scope.organization), string(scope.project), observation, now)
			if err != nil {
				return nil, err
			}
			if changed {
				recorded++
			}
		}
		return struct {
			Recorded int `json:"recorded"`
		}{Recorded: recorded}, nil
	})
}

func validateSpriteUsageRequest(request *spriteUsageRequest, now time.Time) error {
	if len(request.Observations) == 0 || len(request.Observations) > 128 {
		return nativeInvalid("Import between 1 and 128 Sprite usage observations")
	}
	for index := range request.Observations {
		if err := validateSpriteCostObservation(&request.Observations[index], now); err != nil {
			return err
		}
	}
	return nil
}
