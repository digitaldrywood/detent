package usagecosts

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPDeps struct {
	Service            *Service
	Reports            *Service
	Native             func(...hubauth.TokenScope) echo.MiddlewareFunc
	InstanceAdmin      echo.MiddlewareFunc
	Scope              func(echo.Context, bool) hubapi.Scope
	CanManage          func(echo.Context) bool
	HostedOrganization string
	AccessRead         func(context.Context, tracker.OrganizationID, func(tenantstore.Reader) error) error
	Error              func(echo.Context, error) error
	RefuseImport       func(echo.Context) error
}

type HTTP struct{ deps HTTPDeps }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	if deps.Reports == nil {
		deps.Reports = deps.Service
	}
	h := &HTTP{deps: deps}
	const base = "/api/v2/organizations/:organization/projects/:project"
	e.GET(base+"/usage/monthly", h.project, deps.Native(hubauth.Worker, hubauth.Operator, hubauth.Admin))
	e.GET("/api/v2/organizations/:organization/usage/monthly", h.organization, deps.InstanceAdmin)
	e.POST(base+"/usage/sprites", h.ingest, deps.Native(hubauth.Operator, hubauth.Admin))
}

func (h *HTTP) project(c echo.Context) error {
	scope := h.deps.Scope(c, false)
	window, err := Month(c.QueryParam("month"), h.deps.Service.deps.Now())
	if err != nil {
		return h.deps.Error(c, err)
	}
	report, err := h.deps.Reports.Monthly(c.Request().Context(), string(scope.Organization), "project", []string{string(scope.Project)}, window, h.deps.Service.deps.Now())
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (h *HTTP) organization(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	organization := c.Param("organization")
	if h.deps.HostedOrganization != "" && organization != h.deps.HostedOrganization {
		return h.deps.Error(c, hubapi.NotFound())
	}
	err := h.deps.AccessRead(ctx, tracker.OrganizationID(organization), func(r tenantstore.Reader) error { _, err := r.AccessOrganization(ctx); return err })
	if errors.Is(err, corestore.ErrNotFound) {
		return h.deps.Error(c, hubapi.NotFound())
	}
	if err != nil {
		return h.deps.Error(c, err)
	}
	window, err := Month(c.QueryParam("month"), h.deps.Service.deps.Now())
	if err != nil {
		return h.deps.Error(c, err)
	}
	report, err := h.deps.Reports.Monthly(ctx, organization, "organization", nil, window, h.deps.Service.deps.Now())
	if err != nil {
		return h.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, report)
}

func (h *HTTP) ingest(c echo.Context) error {
	if !h.deps.CanManage(c) {
		return h.deps.RefuseImport(c)
	}
	scope := h.deps.Scope(c, true)
	var request SpriteRequest
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return h.deps.Error(c, hubapi.PublicInvalid("Enter sanitized Sprite usage observations"))
	}
	raw, err := h.deps.Service.Import(c.Request().Context(), scope, request, true)
	if err != nil {
		return h.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) Import(ctx context.Context, scope hubapi.Scope, request SpriteRequest, manage bool) ([]byte, error) {
	if !manage {
		return nil, &hubapi.Error{Code: "forbidden", Message: "Importing infrastructure usage requires owner or admin access", Status: http.StatusForbidden, Public: true}
	}
	if err := ValidateSpriteRequest(&request, s.deps.Now()); err != nil {
		return nil, err
	}
	if s.deps.Repository == nil {
		return nil, hubapi.WorkItemStorageUnavailable()
	}
	return s.ImportRecords(ctx, scope, request)
}
