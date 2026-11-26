package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantRequestMetrics = requestmetrics.Service
type requestMetricKey = requestmetrics.Key
type requestLatency = requestmetrics.Latency
type requestMetricsReport = requestmetrics.Report
type routeLatencyBucket = requestmetrics.Bucket
type routeLatencyHistory = requestmetrics.History
type hostedRequestUsage = requestmetrics.Usage
type runnerRequestBudgetContext = requestmetrics.RunnerBudgetContext

func hostedRequestUsageFor(c echo.Context, elapsed time.Duration) hostedRequestUsage {
	return requestmetrics.UsageFor(c, elapsed)
}
func (d *database) metricsBillingOwner() *requestmetrics.BillingOwner {
	return &requestmetrics.BillingOwner{Repository: d.billingRepository, Organization: d.hostedOrganization, Plans: func() *requestmetrics.BillingPlan {
		p := d.plansService().Plans()
		if p == nil {
			return nil
		}
		return &requestmetrics.BillingPlan{WindowSeconds: p.WindowSeconds, RetentionWindows: p.RetentionWindows}
	}}
}
func (d *database) openRequestMetrics(ctx context.Context, org tracker.OrganizationID) (*tenantRequestMetrics, error) {
	if d.postgres == nil || org == "" {
		return nil, ErrHostedDatabaseBinding
	}
	return requestmetrics.New(requestmetrics.Deps{Repository: d.postgres, Organization: org})
}
func (s *Service) requestMetricsHTTP() *requestmetrics.HTTP {
	return requestmetrics.NewHTTP(requestmetrics.HTTPDeps{RequestTimeout: s.requestTimeout, Collector: s.requestMetrics, Repository: s.config.TenantRepository, Now: s.config.Clock, Logger: s.config.Logger, Echo: s.echo, Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, err) }, Refuse: func(c echo.Context, status int, code, reason string, body any) error {
		return s.refusedAPIError(c, status, code, reason, body)
	}, JSONError: s.hostedJSONError, Credential: func(c echo.Context) hubauth.Credential {
		if cred, ok := c.Get("hub_api_credential").(hubapi.APICredential); ok {
			return hubscope.AuthCredential(cred)
		}
		return hubscope.AuthCredential(hubapi.APICredential{})
	}, CurrentOperator: func(ctx context.Context) (hubauth.Credential, error) {
		c, err := currentHubOperator(ctx)
		return hubscope.AuthCredential(c), err
	}, ExplanationRead: hubcontent.ExplanationRead, BillingOwner: s.database.metricsBillingOwner, BindingError: ErrHostedDatabaseBinding})
}
func (s *Service) admitRunnerRequest(c echo.Context, cred hubapi.APICredential) error {
	return s.requestMetricsHTTP().AdmitRunnerRequest(c, hubscope.AuthCredential(cred))
}
func (s *Service) measureRequests(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error { return s.requestMetricsHTTP().Measure(next)(c) }
}
func (s *Service) adminRequestMetrics(ctx context.Context) (requestMetricsReport, bool, error) {
	return s.requestMetricsHTTP().AdminReport(ctx)
}
func (s *Service) adminRouteLatency(ctx context.Context, from, to time.Time, bucket time.Duration) (routeLatencyHistory, bool, error) {
	return s.requestMetricsHTTP().AdminHistory(ctx, from, to, bucket)
}
func (s *Service) hostedRequestMetrics(c echo.Context) error { return s.requestMetricsHTTP().Serve(c) }
func (s *Service) recordHostedRequest(c echo.Context, started time.Time) {
	s.requestMetricsHTTP().RecordHosted(c, started)
}
func (s *Service) expireRequestMetrics(ctx context.Context, now time.Time) error {
	var metricsErr error
	if s.requestMetrics != nil {
		metricsErr = s.requestMetrics.Retain(ctx, now)
	}
	d := s.database
	if d == nil || d.billingRepository == nil || d.hostedOrganization == "" {
		return metricsErr
	}
	owner := d.metricsBillingOwner()
	if owner.Plans() != nil {
		return metricsErr
	}
	defaults := hostedplans.PilotPlans()
	plan := &requestmetrics.BillingPlan{WindowSeconds: defaults.WindowSeconds, RetentionWindows: defaults.RetentionWindows}
	return errors.Join(metricsErr, owner.PruneUsage(ctx, plan, now))
}

func (s *Service) requestTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if s.config.Deps().RequestTimeout == nil {
		return context.WithTimeout(ctx, timeout)
	}
	return s.config.Deps().RequestTimeout(ctx, timeout)
}
