package activity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
)

type HTTPDeps struct {
	Service     func() *Service
	Now         func() time.Time
	Caller      func(context.Context) (Caller, error)
	Available   func() bool
	Unavailable error
	ReadError   func(error) error
	Result      func(any) (operatortool.Result, error)
	Error       func(echo.Context, error) error
	Refuse      func(echo.Context, int, string, string, any) error
}
type HTTPService struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTPService {
	if deps.Unavailable == nil {
		deps.Unavailable = ErrUnavailable
	}
	return &HTTPService{deps: deps}
}
func (s *HTTPService) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, w, err := operatortool.DecodeActivity(call.Arguments, s.deps.Now().UTC())
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := s.deps.Caller(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if !s.deps.Available() {
		return operatortool.Result{}, s.deps.Unavailable
	}
	report, err := s.deps.Service().Read(ctx, credential, r, w)
	if err != nil {
		return operatortool.Result{}, s.deps.ReadError(err)
	}
	return s.deps.Result(report)
}

func (s *HTTPService) Serve(c echo.Context) error {
	r := operatortool.ActivityRequest{ProjectID: c.QueryParam("project_id"), RunnerID: c.QueryParam("runner_id"), From: c.QueryParam("from"), To: c.QueryParam("to")}
	if value := c.QueryParam("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > operatortool.MaxItemLimit {
			return s.deps.Error(c, hubapi.PublicInvalid("Invalid activity limit"))
		}
		r.Limit = limit
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return s.deps.Error(c, err)
	}
	result, err := s.Execute(c.Request().Context(), operatortool.Call{Name: operatortool.Activity, Arguments: raw})
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.deps.Error(c, hubapi.PublicInvalid("Invalid activity filters or window"))
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return s.deps.Refuse(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), hubauth.Response{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
	}
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, result.Content)
}
