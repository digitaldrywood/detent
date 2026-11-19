package executionapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/store"
)

type SpendDeps struct {
	Repository      tenantstore.Repository
	HTTPError       func(echo.Context, error, ...any) error
	AuthorizeAccess func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
}

func (d SpendDeps) GetRunnerSpend(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	var day *time.Time
	if raw := c.QueryParam("day"); raw != "" {
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			return d.HTTPError(c, hubapi.NativePublicInvalid("day must be a UTC calendar date"))
		}
		day = &parsed
	}
	if (day == nil) == (c.QueryParam("issue") == "") {
		return d.HTTPError(c, hubapi.NativePublicInvalid("select exactly one day or issue"))
	}
	var result store.TokenSpend
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(leasestore.RunnerSpendReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var reporter *string
		if scope.Credential.HostedRole == "reporter" {
			reporter = new(scope.Actor().PrincipalID)
		}
		var err error
		result, err = reader.RunnerTokenSpend(ctx, scope.Project, c.QueryParam("issue"), day, reporter)
		return err
	})
	if err != nil {
		if errors.Is(err, leasestore.ErrUnaccountedUsage) {
			return d.HTTPError(c, &hubapi.NativeError{Code: "usage_unaccounted", Message: "Runner usage requires verified historical Hub receipt accounting", Status: http.StatusServiceUnavailable, PublicMessage: true})
		}
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (d SpendDeps) GetRunnerModelQuantiles(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	limit, err := strconv.ParseInt(c.QueryParam("limit"), 10, 64)
	if err != nil || limit < 1 || limit > 200 || c.QueryParam("model") == "" {
		return d.HTTPError(c, hubapi.NativePublicInvalid("model and a limit from 1 to 200 are required"))
	}
	var result store.ModelTokenQuantiles
	err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(leasestore.RunnerSpendReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var reporter *string
		if scope.Credential.HostedRole == "reporter" {
			reporter = new(scope.Actor().PrincipalID)
		}
		var err error
		result, err = reader.RunnerModelTokenQuantiles(ctx, scope.Project, store.ModelTokenQuantileQuery{Model: c.QueryParam("model"), Limit: limit}, reporter)
		return err
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
