package requestmetrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
)

type HTTPDeps struct {
	Collector       *Service
	Repository      tenantstore.Repository
	Now             func() time.Time
	Logger          *slog.Logger
	Echo            *echo.Echo
	Error           func(echo.Context, error) error
	Refuse          func(echo.Context, int, string, string, any) error
	JSONError       func(echo.Context, int, string) error
	Credential      func(echo.Context) hubauth.Credential
	CurrentOperator func(context.Context) (hubauth.Credential, error)
	ExplanationRead func(context.Context) bool
	BillingOwner    func() *BillingOwner
	BindingError    error
	RequestTimeout  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}
type HTTP struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTP { return &HTTP{deps: deps} }

type apiErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *HTTP) AdmitRunnerRequest(c echo.Context, credential hubauth.Credential) error {
	requestTimeout := s.deps.RequestTimeout
	if requestTimeout == nil {
		requestTimeout = context.WithTimeout
	}
	ctx, cancel := requestTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	m := s.deps.Collector
	if m == nil || credential.Runner.RunnerID == "" {
		return nil
	}
	c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), RunnerBudgetContext{}, true)))
	if RunnerExempt(c.Request().Method, c.Path()) {
		return nil
	}
	var capacity int

	if s.deps.Repository == nil || m.Organization == "" {
		return s.deps.Error(c, s.deps.BindingError)
	}
	if credential.RequestCapacity != nil {
		capacity = *credential.RequestCapacity
	} else {
		var runnerCapacity int64
		err := s.deps.Repository.ReadTenant(ctx, m.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			identity, err := reader.RunnerIdentity(ctx, credential.Runner.RunnerID)
			if err != nil {
				return err
			}
			machine, err := reader.Machine(ctx, string(identity.MachineID))
			if err != nil {
				return err
			}
			runnerCapacity = min(identity.CapacityLimit, machine.Capacity)
			return nil
		})
		if err != nil {
			return s.deps.Error(c, err)
		}
		capacity = int(runnerCapacity)

	}

	projects := make([]string, 0, len(credential.Runner.ProjectIDs))
	for _, id := range credential.Runner.ProjectIDs {
		projects = append(projects, string(id))
	}
	now := s.deps.Now().UTC()
	if retry := m.Admit(now, credential.Runner.RunnerID, c.Request().Method+" "+c.Path(), capacity, projects); retry > 0 {
		c.Response().Header().Set("Retry-After", strconv.Itoa(retry))
		return s.deps.Refuse(c, http.StatusTooManyRequests, "runner_request_budget", "Runner request budget exceeded; retry after the current minute", apiErrorResponse{Code: "runner_request_budget", Message: "Runner request budget exceeded; retry after the current minute"})
	}
	return nil
}

func (s *HTTP) Measure(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		err := next(c)
		if s.deps.Collector == nil {
			return err
		}
		credential := s.deps.Credential(c)
		kind, caller := Caller(credential)
		if kind == "anonymous" {
			if session, ok := c.Get("hosted_session").(auth.Session); ok && session.Identity != nil {
				kind, caller = "browser", session.Identity.Subject
			}
		}
		status := c.Response().Status
		if err != nil {
			status = 500
			var failure *echo.HTTPError
			if errors.As(err, &failure) {
				status = failure.Code
			}
		}
		s.deps.Collector.Record(s.deps.Now(), Route(s.deps.Collector.KnownRoutes(s.deps.Echo), c.Request().Method, c.Path()), kind, caller, status, time.Since(start))
		return err
	}
}

func (s *HTTP) Visible(ctx context.Context) bool {
	if s.deps.Collector == nil {
		return false
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationID: string(s.deps.Collector.Organization), OrganizationWide: true}); err != nil {
		return false
	}
	credential, err := s.deps.CurrentOperator(ctx)
	return err == nil && (credential.HostedKeyScope == "" || credential.HostedProjectAccess == "all")
}

func (s *HTTP) AdminReport(ctx context.Context) (Report, bool, error) {
	if !s.Visible(ctx) {
		return Report{}, false, nil
	}
	report, err := s.deps.Collector.Report(ctx, s.deps.Now())
	return report, true, err
}
func (s *HTTP) AdminHistory(ctx context.Context, from, to time.Time, bucket time.Duration) (History, bool, error) {
	if !s.Visible(ctx) {
		return History{}, false, nil
	}
	history, err := s.deps.Collector.History(ctx, from, to, bucket)
	return history, true, err
}

type Response struct {
	Report
	RouteLatency *History `json:"route_latency,omitempty"`
}

func (s *HTTP) Serve(c echo.Context) error {
	ctx := c.Request().Context()
	if !s.Visible(ctx) {
		return s.deps.JSONError(c, http.StatusForbidden, "Staff access is required to read request metrics")
	}
	var window time.Duration
	if raw := c.QueryParam("window"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Hour || parsed > requestHourRetention {
			return s.deps.JSONError(c, http.StatusBadRequest, "window must be a duration between 1h and 2160h")
		}
		window = parsed
	}
	now := s.deps.Now().UTC()
	report, err := s.deps.Collector.Report(ctx, now)
	response := Response{Report: report}
	if err == nil && window > 0 {
		var history History
		history, err = s.deps.Collector.History(ctx, now.Add(-window), now, time.Hour)
		response.RouteLatency = &history
	}
	if err != nil {
		s.deps.Logger.Warn("read request metrics", "error", err)
		return s.deps.JSONError(c, http.StatusServiceUnavailable, "Request metrics are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, response)
}
func (s *HTTP) RecordHosted(c echo.Context, started time.Time) {
	m := s.deps.Collector
	if m == nil || s.deps.ExplanationRead(c.Request().Context()) {
		return
	}
	credential := s.deps.Credential(c)
	kind, caller := Caller(credential)
	if kind == "anonymous" {
		if session, ok := c.Get("hosted_session").(auth.Session); ok && session.Identity != nil {
			kind, caller = "browser", session.Identity.Subject
		}
	}
	now := s.deps.Now()
	key := Key{now.Unix() / 60 * 60, Route(m.KnownRoutes(s.deps.Echo), c.Request().Method, c.Path()), kind, caller, c.Response().Status / 100}
	usage := UsageFor(c, time.Since(started))
	m.RecordUsage(s.deps.BillingOwner(), key, usage)
}
