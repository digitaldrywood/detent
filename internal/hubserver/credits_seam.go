package hubserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"

	"encoding/csv"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/credits"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerHostedCreditWriteRoutes(e *echo.Echo) {
	e.POST("/organization/billing/credits/checkout", s.hostedCreditCheckout)
	e.POST("/organization/billing/credits/auto-fund", s.hostedCreditAutoFund)
	e.POST(hostedOrganizationBase+"/billing/credits/checkout", s.hostedCreditCheckout)
	e.PUT(hostedOrganizationBase+"/billing/credits/auto-fund", s.hostedCreditAutoFund)
	e.POST("/webhooks/stripe", s.hostedStripeWebhook)
}

func (s *Service) hostedCreditCheckout(c echo.Context) error {
	api := hostedBillingAPI(c)
	if _, err := s.hostedBillingOwner(c.Request().Context(), c); err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	var request struct {
		hostedIdempotent
		Price string `json:"price"`
	}
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		if err := request.validate(true); err != nil {
			return s.nativeAPIError(c, err)
		}
	} else {
		request.Price = c.FormValue("price")
	}
	authorize := func(ctx context.Context) (apiCredential, error) { return s.hostedBillingOwner(ctx, c) }
	result, err := s.checkoutCredits(c.Request().Context(), authorize, request.Price, request.IdempotencyKey)
	if err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	return s.hostedBillingDestination(c, api, result.URL)
}

func (s *Service) hostedCreditAutoFund(c echo.Context) error {
	api := hostedBillingAPI(c)
	if _, err := s.hostedBillingOwner(c.Request().Context(), c); err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	var request creditFundingInput
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
	} else {
		request.Enabled = c.FormValue("enabled") == "on"
		request.Price = c.FormValue("price")
		threshold, err := strconv.ParseInt(c.FormValue("threshold_cents"), 10, 64)
		if err != nil && request.Enabled {
			return s.hostedBillingFailure(c, api, http.StatusBadRequest, "Enter a threshold in USD cents")
		}
		request.Threshold = threshold
	}
	authorize := func(ctx context.Context) (apiCredential, error) { return s.hostedBillingOwner(ctx, c) }
	if _, err := s.configureCreditFunding(c.Request().Context(), authorize, request, ""); err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	if api {
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusSeeOther, s.hostedBillingReturn(true))
}

func (s *Service) creditCommandFailure(c echo.Context, api bool, err error) error {
	s.logStripeFailure(c.Request().Context(), err)
	var failure *nativeError
	switch {
	case errors.Is(err, auth.ErrHostedIdentity):
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "AI credits require an organization owner without support impersonation")
	case errors.Is(err, mutation.ErrConflict), errors.Is(err, mutation.ErrUncertain):
		return s.hostedBillingFailure(c, api, http.StatusConflict, err.Error())
	case errors.As(err, &failure):
		return s.hostedBillingFailure(c, api, failure.status, failure.Message)
	default:
		return s.nativeAPIError(c, err)
	}
}

func creditPageQuery(c echo.Context) (creditPageRequest, error) {
	p := creditPageRequest{Cursor: c.QueryParam("cursor"), From: c.QueryParam("from"), To: c.QueryParam("to")}
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return p, nativeInvalid("credit transaction limit must be between 1 and 100")
		}
		p.Limit = n
	}
	return p, nil
}

func (s *Service) hostedCreditTransactions(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c.Request().Context(), c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	p, err := creditPageQuery(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if c.QueryParam("format") == "csv" {
		return s.exportCreditTransactions(ctx, c, credential, p)
	}
	view, err := s.readAICredits(ctx, p)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *Service) exportCreditTransactions(ctx context.Context, c echo.Context, credential apiCredential, p creditPageRequest) error {
	from, to, err := p.bounds()
	if err != nil {
		return s.nativeAPIError(c, err)
	}

	return s.exportPostgresCreditTransactions(ctx, c, credential, from, to)
}

func (s *Service) creditCSVWriter(ctx context.Context, c echo.Context, credential apiCredential) (*csv.Writer, error) {
	auditCtx := mutation.WithContext(ctx, mutation.Metadata{Action: "billing_export", Source: "http", CorrelationID: s.config.newLeaseID()})
	if err := s.hostedAudit(auditCtx, credential.Hosted, "billing_exported", c.Path(), "", http.StatusOK); err != nil {
		return nil, s.nativeAPIError(c, err)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/csv; charset=utf-8")
	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="credit-transactions.csv"`)
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	writer := csv.NewWriter(c.Response())
	if err := writer.Write([]string{"recorded_at", "kind", "turn_id", "tokens", "charged_amount_usd", "included_amount_usd"}); err != nil {
		return nil, err
	}
	return writer, nil
}

func (s *Service) exportPostgresCreditTransactions(ctx context.Context, c echo.Context, credential apiCredential, from, to int64) error {
	started, err := s.creditsService().Export(ctx, from, to, func() (*csv.Writer, error) { return s.creditCSVWriter(ctx, c, credential) })
	if !started && err != nil {
		return s.nativeAPIError(c, fromFeatureError(err))
	}
	return err
}
func (s *Service) platformTenantAICreditBalance(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.hostedJSONError(c, http.StatusForbidden, "Shared service access is required to read AI credits")
	}
	result, err := s.database.creditsService().PlatformBalance(ctx)
	if err != nil {
		return s.internalAPIError(c, "ai_credits_unavailable", "AI credits are unavailable", fromFeatureError(err))
	}
	return c.JSON(http.StatusOK, result)
}
func (s *Service) adjustPlatformAICredits(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" || claims.Email == "" {
		return s.hostedJSONError(c, http.StatusForbidden, "Shared service identity and email are required to adjust AI credits")
	}
	var adjustment billing.CreditAdjustment
	if err := decodeAPIJSON(c, &adjustment); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	balance, err := s.database.adjustAICredits(c.Request().Context(), claims.Email, adjustment)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"organization_id": s.database.hostedOrganization, "balance_micros": balance})
}

func (s *Service) chatUsageRows(ctx context.Context, window usageWindow, projects []string) ([]usageRow, error) {
	if s.database.billingRepository == nil {
		return nil, errors.New("billing repository is unavailable")
	}
	if len(projects) == 0 {
		return []usageRow{}, nil
	}
	buckets, err := s.database.creditsService().UsageBuckets(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), credits.Window(window), projects)
	if err != nil {
		return nil, fromFeatureError(err)
	}
	if len(projects) == 0 {
		return []usageRow{}, nil
	}
	var rows []usageRow
	for _, bucket := range buckets {
		rows = append(rows, usageRow{Chat: true, ChatSessions: bucket.Turns, Period: bucket.Period, Provider: bucket.Provider, Model: bucket.Model, Input: bucket.Input, CachedInput: bucket.CachedInput, Output: bucket.Output, Cost: bucket.CostUSD, CacheSavings: bucket.CacheSavingsUSD, Currency: "USD"})
	}
	return rows, nil
}

func (plan HostedPlan) includedAICreditMicros() *int64 {
	amount := int64(0)
	tier, ok := hostedTierFor(plan.PlanReference)
	if !ok || !tier.offered || plan.ID == "free" {
		return &amount
	}
	if plan.MonthlyUSDCents == nil {
		return nil
	}
	amount = max(0, *plan.MonthlyUSDCents) * 1000
	return &amount
}

type aiCreditTransaction = credits.Transaction
type aiCreditView = credits.View
type includedAICredits credits.Included
type creditPageRequest credits.Page
type preparedConversationUsage = credits.PreparedUsage

type chatUsageSummary struct {
	Range           usageWindow `json:"range"`
	Input           int64       `json:"input"`
	CachedInput     int64       `json:"cached_input"`
	Output          int64       `json:"output"`
	ReasoningOutput int64       `json:"reasoning_output"`
	Tokens          int64       `json:"tokens"`
	Turns           int64       `json:"turns"`
	UnpricedTurns   int64       `json:"unpriced_turns"`
	CostUSD         float64     `json:"cost_usd"`
}

func (c includedAICredits) remaining() int64 { return credits.Included(c).Remaining() }
func (p creditPageRequest) bounds() (int64, int64, error) {
	from, to, err := credits.Page(p).Bounds()
	return from, to, fromFeatureError(err)
}

func (d *database) creditDeps(deps credits.Deps) credits.Deps {
	deps.Repository = d.billingRepository
	deps.Organization = d.hostedOrganization
	deps.Mode = d.aiCreditMode
	deps.PaymentNow = d.now
	deps.CostMultiplier = d.aiCreditCostMultiplier
	deps.Details = d.billingAuditStore
	deps.ReadReason = d.readBillingReason
	deps.ArchiveReason = d.archiveBillingReason
	deps.SafeID = hostedSafeID
	deps.ValidateDetail = validateNativeDetailReference
	deps.ReadIncluded = d.plans() != nil
	deps.Plan = func(ctx context.Context, r billingstore.Reader, ref billingstore.PlanReference) (credits.Plan, error) {
		plan, err := d.readHostedPlanRecord(ctx, r, PlanReference{ID: ref.ID, Version: ref.Version})
		return credits.Plan{Name: plan.Name, AllowanceMicros: plan.includedAICreditMicros()}, err
	}
	return deps
}
func (d *database) creditsService() *credits.Service {
	return credits.New(d.creditDeps(credits.Deps{Now: d.now}))
}

func (s *Service) creditsService() *credits.Service {
	deps := credits.Deps{
		Repository: s.database.billingRepository, Organization: s.database.hostedOrganization,
		ReadIncluded: s.database.plans() != nil, Mode: s.database.aiCreditMode, Now: s.config.now, ReadReason: s.database.readBillingReason,
		Plan: func(ctx context.Context, r billingstore.Reader, ref billingstore.PlanReference) (credits.Plan, error) {
			plan, err := s.database.readHostedPlanRecord(ctx, r, PlanReference{ID: ref.ID, Version: ref.Version})
			return credits.Plan{Name: plan.Name, AllowanceMicros: plan.includedAICreditMicros()}, err
		},
	}
	if cfg := s.hostedBillingConfig(); cfg != nil {
		for _, pack := range cfg.CreditPacks {
			deps.Packs = append(deps.Packs, credits.Pack(pack))
		}
	}
	return credits.New(deps)
}

func (s *Service) readAICredits(ctx context.Context, pages ...creditPageRequest) (*aiCreditView, error) {
	requests := make([]credits.Page, len(pages))
	for i, page := range pages {
		requests[i] = credits.Page(page)
	}
	value, err := s.creditsService().Read(ctx, requests...)
	return value, fromFeatureError(err)
}

func (s *Service) readCreditHistory(ctx context.Context, view *aiCreditView, page creditPageRequest) error {
	return fromFeatureError(s.creditsService().History(ctx, view, credits.Page(page)))
}

func (d *database) includedAICredits(ctx context.Context, _ nativeQueryer, organization tracker.OrganizationID, at time.Time) (includedAICredits, error) {
	if d.plans() == nil {
		return includedAICredits{}, nil
	}
	return d.includedAICreditsRecord(ctx, organization, at)
}

func (d *database) includedAICreditsRecord(ctx context.Context, organization tracker.OrganizationID, at time.Time) (includedAICredits, error) {
	value, err := d.creditsService().Included(ctx, organization, at)
	return includedAICredits(value), fromFeatureError(err)
}

func (d *database) includedAICreditsFromReader(ctx context.Context, r billingstore.Reader, at time.Time) (includedAICredits, error) {
	value, err := d.creditsService().IncludedFromReader(ctx, r, at)
	return includedAICredits(value), fromFeatureError(err)
}

func (d *database) applyCreditPaymentRecord(ctx context.Context, w billingstore.Writer, payment billing.CreditPayment) error {
	return fromFeatureError(d.creditsService().ApplyPayment(ctx, w, payment))
}

func (d *database) adjustAICredits(ctx context.Context, actor string, adjustment billing.CreditAdjustment) (int64, error) {
	value, err := d.creditsService().Adjust(ctx, actor, adjustment)
	return value, fromFeatureError(err)
}

func (d *database) RecordConversationUsage(ctx context.Context, usage ConversationUsage) error {
	if d.billingRepository == nil {
		return errors.New("billing repository is unavailable")
	}
	return d.recordConversationUsagePostgres(ctx, usage)
}

func (d *database) recordConversationUsagePostgres(ctx context.Context, usage ConversationUsage) error {
	return fromFeatureError(d.creditsService().RecordUsage(ctx, credits.Usage(usage)))
}

func (d *database) prepareConversationUsagePostgres(ctx context.Context, usage ConversationUsage) (preparedConversationUsage, error) {
	value, err := d.creditsService().PrepareUsage(ctx, credits.Usage(usage))
	return value, fromFeatureError(err)
}

func (d *database) writeConversationUsagePostgres(ctx context.Context, w tenantstore.Writer, prepared preparedConversationUsage) error {
	return fromFeatureError(d.creditsService().WriteUsage(ctx, w, prepared))
}

func (d *database) conversationUsageRecord(ctx context.Context, r billingstore.Reader, usage ConversationUsage) (billingstore.ConversationUsage, error) {
	value, err := d.creditsService().UsageRecord(ctx, r, credits.Usage(usage))
	return value, fromFeatureError(err)
}

func conversationUsageCostObservation(row billingstore.ConversationUsage) (costObservation, error) {
	return credits.UsageCostObservation(row)
}
func chatUsageCost(price UsagePrice, tokens runner.AgentTokenCounts) float64 {
	return credits.UsageCost(billingstore.ConversationPrice{Input: price.Input, CachedInput: price.CachedInput, Output: price.Output}, tokens)
}
func chatBillingWindow(now time.Time, subscription billing.Snapshot) usageWindow {
	return usageWindow(credits.BillingWindow(now, subscription))
}
func (d *database) chatUsageSummary(ctx context.Context, organization tracker.OrganizationID, window usageWindow, projects []string) (chatUsageSummary, error) {
	value, err := d.creditsService().UsageSummary(ctx, organization, credits.Window(window), projects)
	return chatUsageSummary{Range: usageWindow(value.Range), Input: value.Input, CachedInput: value.CachedInput, Output: value.Output, ReasoningOutput: value.ReasoningOutput, Tokens: value.Tokens, Turns: value.Turns, UnpricedTurns: value.UnpricedTurns, CostUSD: value.CostUSD}, fromFeatureError(err)
}
func creditCSVCell(value string) string { return credits.CSVCell(value) }
func writeCreditCSVEntry(writer *csv.Writer, row billingstore.CreditExportEntry) error {
	return credits.WriteCSVEntry(writer, row)
}

func (s *Service) creditPack(id string) (HostedCreditPack, bool) {
	value, ok := s.creditWorkerService().CreditPack(id)
	return HostedCreditPack(value), ok
}
func (s *Service) creditWorkerService() *credits.Service {
	cfg := s.hostedBillingConfig()
	var funding *credits.Funding
	if cfg != nil {
		provider, _ := cfg.Provider.(billing.CreditProvider)
		funding = &credits.Funding{Provider: provider, CheckoutDisabled: cfg.CheckoutDisabled}
		for _, pack := range cfg.CreditPacks {
			funding.CreditPacks = append(funding.CreditPacks, credits.Pack(pack))
		}
	}
	return credits.New(s.database.creditDeps(credits.Deps{Now: s.config.now, Funding: funding, NewID: s.config.newLeaseID, Binding: func(ctx context.Context) (billing.Binding, error) { return s.database.hostedBillingBinding(ctx, cfg) }, Unbound: errHostedBillingUnbound}))
}
func (w *hostedBillingWorker) creditEvent(ctx context.Context, id, kind string) error {
	return fromFeatureError(w.service.creditWorkerService().Event(ctx, id, kind))
}
func (w *hostedBillingWorker) autoFund(ctx context.Context) error {
	return fromFeatureError(w.service.creditWorkerService().AutoFund(ctx))
}
