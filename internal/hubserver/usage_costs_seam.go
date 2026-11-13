package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/usagecosts"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const costTimeLayout = usagecosts.TimeLayout
const spriteCostBucket = usagecosts.SpriteBucket
const runnerCostBucket = usagecosts.RunnerBucket
const lunaCostBucket = usagecosts.LunaBucket

type usageWindow = usagecosts.Window
type costObservation = usagecosts.Observation
type attributedCostObservation = usagecosts.AttributedObservation
type monthlyCostTotal = usagecosts.Total
type monthlyCostReport = usagecosts.Report
type costResource = usagecosts.Resource
type runnerUsageFacts = usagecosts.RunnerFacts
type spriteUsageRequest = usagecosts.SpriteRequest

func normalizeCostObservation(o *costObservation, now time.Time) error {
	return fromFeatureError(usagecosts.Normalize(o, now))
}
func costMonth(value string, now time.Time) (usageWindow, error) {
	v, e := usagecosts.Month(value, now)
	return v, fromFeatureError(e)
}
func buildMonthlyCostReport(organization, scope string, window usageWindow, now time.Time, observations []attributedCostObservation, resources []costResource) (monthlyCostReport, error) {
	v, e := usagecosts.BuildReport(organization, scope, window, now, observations, resources)
	return v, fromFeatureError(e)
}
func boundedMonthlyCostDetails(report monthlyCostReport) monthlyCostReport {
	return usagecosts.BoundedDetails(report)
}
func validateSpriteUsageRequest(request *spriteUsageRequest, now time.Time) error {
	return fromFeatureError(usagecosts.ValidateSpriteRequest(request, now))
}
func validateRunnerCostUsage(entry tracker.NativeUsage) error {
	return fromFeatureError(usagecosts.ValidateRunnerUsage(entry))
}
func nativeUsageCorrection(previous, next tracker.NativeRunData) bool {
	return usagecosts.UsageCorrection(previous, next)
}
func (d *database) costService() *usagecosts.Service {
	return usagecosts.New(usagecosts.Deps{Repository: d.billingRepository, Details: d.billingAuditStore})
}
func (d *database) monthlyCosts(ctx context.Context, organization, scope string, projects []string, window usageWindow, now time.Time) (monthlyCostReport, error) {
	v, e := d.costService().Monthly(ctx, organization, scope, projects, window, now)
	return v, fromFeatureError(e)
}
func (s *Service) costService() *usagecosts.Service {
	deps := usagecosts.Deps{Repository: s.config.TenantRepository, Details: s.runDetailStore(), Now: s.config.now, Prices: usagecosts.Prices(s.usagePrices()), ContentContext: func(ctx context.Context) context.Context { return isolateContentContext(s.runDetailContext(ctx)) }, RecordError: projectRecordError, ValidateDetail: validateNativeDetailReference}
	if s.database != nil && s.database.billingRepository != nil {
		deps.RequireCollaboration = func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
			return s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now)
		}
	}
	deps.AccountUsage = func(ctx context.Context, w tenantstore.Writer, now time.Time, id string) error {
		return s.applyRunnerTenantUsage(ctx, w, now, id, false, false)
	}
	return usagecosts.New(deps)
}
func (s *Service) prepareRunnerCostRecords(ctx context.Context, scope nativeScope, request tracker.NativeRunEvent, facts runnerUsageFacts, now time.Time, historical ...billingstore.CostObservation) ([]billingstore.CostObservation, error) {
	v, e := s.costService().PrepareRunnerRecords(ctx, s.featureScope(scope), request, facts, now, historical...)
	return v, fromFeatureError(e)
}

func (s *Service) registerCostUsageRoutes(e *echo.Echo) {
	if s.config.Hosted == nil {
		e.GET("/api/v2/organizations/:organization/monthly-budget", s.instanceMonthlyBudgetScope, s.requireInstanceAdmin())
		e.PUT("/api/v2/organizations/:organization/monthly-budget", s.instanceMonthlyBudgetScope, s.requireInstanceAdmin())
	}
	e.GET(nativeBase+"/monthly-budget", s.getMonthlyBudget, s.requireNativeScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin))
	e.PUT(nativeBase+"/monthly-budget", s.updateMonthlyBudget, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	hosted := ""
	if s.config.Hosted != nil {
		hosted = s.config.Hosted.OrganizationID
	}
	usagecosts.RegisterRoutes(e.Group(""), usagecosts.HTTPDeps{Service: s.costService(), Reports: s.database.costService(), Native: func(roles ...hubauth.TokenScope) echo.MiddlewareFunc {
		allowed := make([]apiScope, len(roles))
		for i, role := range roles {
			allowed[i] = apiScope(role)
		}
		return s.requireNativeScope(allowed...)
	}, InstanceAdmin: s.requireInstanceAdmin(), HostedOrganization: hosted, AccessRead: s.accessRead, Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
		Scope: func(c echo.Context, admin bool) hubapi.Scope {
			scope := nativeRequestScope(c)
			if admin {
				scope.requireHostedAdmin = true
				c.Set("native_scope", scope)
			}
			return s.featureScope(scope)
		},
		CanManage: func(c echo.Context) bool { return canManageProjectSecrets(nativeRequestScope(c).credential) },
		RefuseImport: func(c echo.Context) error {
			return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Importing infrastructure usage requires owner or admin access", apiErrorResponse{Code: "forbidden", Message: "Importing infrastructure usage requires owner or admin access"})
		}})
}
func (s *Service) importSpriteUsageCommand(ctx context.Context, scope nativeScope, request spriteUsageRequest) ([]byte, error) {
	manage := canManageProjectSecrets(scope.credential)
	scope.requireHostedAdmin = true
	raw, err := s.costService().Import(ctx, s.featureScope(scope), request, manage)
	return raw, fromFeatureError(err)
}

type UsagePrice = usagecosts.Price
type UsageConfig usagecosts.Prices

const defaultUsageCurrency = usagecosts.DefaultCurrency

func (c UsageConfig) normalized() UsageConfig { return UsageConfig(usagecosts.Prices(c).Normalized()) }
func (c UsageConfig) price(model string) (UsagePrice, bool) {
	return usagecosts.Prices(c).PriceFor(model)
}
func (c UsageConfig) estimate(model string, input, cachedInput, output int64) (float64, string, bool) {
	return usagecosts.Prices(c).Estimate(model, input, cachedInput, output)
}
func (c UsageConfig) cacheSaving(model string, input, cachedInput int64) float64 {
	return usagecosts.Prices(c).CacheSaving(model, input, cachedInput)
}
func (c UsageConfig) currency() string { return usagecosts.Prices(c).CurrencyValue() }
func (s *Service) usagePrices() UsageConfig {
	if s.config.Usage == nil {
		return UsageConfig{Currency: defaultUsageCurrency}
	}
	return *s.config.Usage
}
