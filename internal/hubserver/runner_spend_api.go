package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/store"
)

func (s *Service) getRunnerSpend(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	var day *time.Time
	if raw := c.QueryParam("day"); raw != "" {
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			return s.nativeAPIError(c, nativePublicInvalid("day must be a UTC calendar date"))
		}
		day = &parsed
	}
	if (day == nil) == (c.QueryParam("issue") == "") {
		return s.nativeAPIError(c, nativePublicInvalid("select exactly one day or issue"))
	}
	var result store.TokenSpend
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(leasestore.RunnerSpendReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		result, err = reader.RunnerTokenSpend(ctx, scope.project, c.QueryParam("issue"), day)
		return err
	})
	if err != nil {
		if errors.Is(err, leasestore.ErrUnaccountedUsage) {
			return s.nativeAPIError(c, &nativeError{Code: "usage_unaccounted", Message: "Runner usage requires verified historical Hub receipt accounting", status: http.StatusServiceUnavailable, publicMessage: true})
		}
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) getRunnerModelQuantiles(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	limit, err := strconv.ParseInt(c.QueryParam("limit"), 10, 64)
	if err != nil || limit < 1 || limit > 200 || c.QueryParam("model") == "" {
		return s.nativeAPIError(c, nativePublicInvalid("model and a limit from 1 to 200 are required"))
	}
	var result store.ModelTokenQuantiles
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(leasestore.RunnerSpendReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		result, err = reader.RunnerModelTokenQuantiles(ctx, scope.project, store.ModelTokenQuantileQuery{Model: c.QueryParam("model"), Limit: limit})
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
