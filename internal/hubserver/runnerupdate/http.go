package runnerupdate

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerenroll"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HTTPDeps struct {
	Service    func() *Service
	Auth       func() *hubauth.Authenticator
	Credential func(echo.Context) (hubauth.Credential, bool)
	Admin      echo.MiddlewareFunc
	Worker     echo.MiddlewareFunc
	Error      func(echo.Context, error) error
	Invalid    func(echo.Context, error) error
}

type httpRoutes struct{ deps HTTPDeps }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	if deps.Credential == nil {
		deps.Credential = hubauth.RequestCredential
	}
	s := &httpRoutes{deps: deps}
	base := "/api/v2/organizations/:organization/runners/:runner/update"
	urgent := "/api/v2/organizations/:organization/runner-update/urgent"
	e.POST("/api/v2/organizations/:organization/runners/:runner/lifecycle", s.reportRunnerLifecycle, deps.Worker)
	e.GET(base+"/restart-evidence", s.getRunnerRollout, deps.Admin)
	e.POST(base+"/restart-evidence", s.reportRunnerRestartEvidence, deps.Admin)
	e.GET(base+"/target", s.getRunnerTarget, deps.Admin)
	e.GET("/api/v2/organizations/:organization/runner-update/target", s.getRunnerTarget, deps.Admin)
	e.PUT(base+"/target", s.setRunnerTarget, deps.Admin)
	e.PUT("/api/v2/organizations/:organization/runner-update/target", s.setRunnerTarget, deps.Admin)
	e.GET(base, s.getRunnerUpdate, deps.Admin)
	e.POST(base+"/apply", s.applyRunnerUpdate, deps.Admin)
	e.GET(base+"/receipt", s.getRunnerUpdateReceipt, deps.Worker)
	e.PUT(base+"/receipt", s.putRunnerUpdateReceipt, deps.Worker)
	e.GET(urgent, s.getUrgentRunnerUpdate, deps.Admin)
	e.PUT(urgent, s.markUrgentRunnerUpdate, deps.Admin)
}

func (s *httpRoutes) scope(c echo.Context, credential hubauth.Credential) hubapi.Scope {
	return s.authority(c, credential, false)
}

func (s *httpRoutes) authority(c echo.Context, credential hubauth.Credential, admin bool) hubapi.Scope {
	scope := s.deps.Auth().Scope(hubauth.Input{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential, Admin: admin})
	if credential.HostedKeyScope != "" && credential.HostedProjectAccess != "all" || credential.EntryKey != nil && (credential.EntryKey.Access != "all" || len(credential.EntryKey.BlockedProjects) > 0) {
		return hubapi.NewScope(scope.Facts, func(context.Context, accessstore.Reader, bool) error {
			return &hubapi.Error{Code: "insufficient_scope", Message: "Runner updates require access to all projects", Status: http.StatusForbidden, Public: true}
		})
	}
	return scope
}

func (s *httpRoutes) getRunnerUpdate(c echo.Context) error {
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, hubapi.NotFound())
	}
	value, err := s.deps.Service().View(c.Request().Context(), s.scope(c, credential), c.Param("runner"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *httpRoutes) applyRunnerUpdate(c echo.Context) error {
	var request Change
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return s.deps.Invalid(c, err)
	}
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, hubapi.NotFound())
	}
	value, err := s.deps.Service().Apply(c.Request().Context(), s.scope(c, credential), c.Param("runner"), request)
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusAccepted, value)
}

func (s *httpRoutes) getUrgentRunnerUpdate(c echo.Context) error {
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, hubapi.NotFound())
	}
	scope := s.scope(c, credential)
	value, err := s.deps.Service().ReadUrgent(c.Request().Context(), scope)
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *httpRoutes) markUrgentRunnerUpdate(c echo.Context) error {
	var request UrgentChange
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return s.deps.Invalid(c, err)
	}
	credential, ok := s.deps.Credential(c)
	if !ok {
		return s.deps.Error(c, hubapi.NotFound())
	}
	scope := s.scope(c, credential)
	value, err := s.deps.Service().MarkUrgent(c.Request().Context(), scope, request)
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusAccepted, value)
}

func (s *httpRoutes) runnerReceiptScope(c echo.Context) (hubapi.Scope, error) {
	credential, ok := s.deps.Credential(c)
	if !ok || credential.Runner.RunnerID == "" || credential.Runner.RunnerID != c.Param("runner") || string(credential.Runner.OrganizationID) != c.Param("organization") {
		return hubapi.Scope{}, runnerenroll.Unauthorized()
	}
	return s.scope(c, credential), nil
}

func (s *httpRoutes) getRunnerUpdateReceipt(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	receipt, err := s.deps.Service().Receipt(c.Request().Context(), scope, c.Param("runner"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, receipt)
}

func (s *httpRoutes) putRunnerUpdateReceipt(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	var request ReceiptWrite
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return s.deps.Invalid(c, err)
	}
	if err := s.deps.Service().PutReceipt(c.Request().Context(), scope, c.Param("runner"), request); err != nil {
		return s.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
