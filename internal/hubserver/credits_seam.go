package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
	"github.com/digitaldrywood/detent/internal/mutation"

	"github.com/digitaldrywood/detent/internal/hubserver/credits"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) registerHostedCreditWriteRoutes(e *echo.Echo) {
	credits.RegisterWriteRoutes(e.Group(""), s.creditHTTPDeps())
	e.POST("/webhooks/stripe", s.hostedStripeWebhook)
}

func (s *Service) registerHostedCreditReadRoutes(e *echo.Echo) {
	credits.RegisterReadRoutes(e.Group(""), s.creditHTTPDeps())
}

func (s *Service) registerPlatformCreditRoutes(e *echo.Echo) {
	credits.RegisterPlatformRoutes(e.Group(""), s.creditHTTPDeps())
}

func (s *Service) creditHTTPDeps() credits.HTTPDeps {
	return credits.HTTPDeps{
		Service: s.creditsService, PlatformService: func() *credits.Service { return s.database.creditsService() }, Auth: s.requestAuth,
		Error:          func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
		InvalidRequest: s.invalidAPIRequest, CommandFailure: s.creditCommandFailure,
		Failure: s.hostedBillingFailure, Destination: s.hostedBillingDestination,
		Return:      func() string { return s.hostedBillingReturn(true) },
		ValidateKey: func(key string) error { return (hostedIdempotent{IdempotencyKey: key}).validate(true) },
		Checkout: func(ctx context.Context, authorize func(context.Context) (hubauth.Credential, error), price, key string) (string, error) {
			value, err := s.checkoutCredits(ctx, func(ctx context.Context) (apiCredential, error) { return creditCredential(authorize(ctx)) }, price, key)
			return value.URL, err
		},
		ConfigureFunding: func(ctx context.Context, authorize func(context.Context) (hubauth.Credential, error), request credits.FundingInput, key string) (credits.FundingInput, error) {
			return s.configureCreditFunding(ctx, func(ctx context.Context) (apiCredential, error) { return creditCredential(authorize(ctx)) }, request, key)
		},
		NewID: s.config.newLeaseID,
		Audit: func(ctx context.Context, credential hubauth.Credential, c echo.Context) error {
			return s.hostedAudit(ctx, credential.Hosted, "billing_exported", c.Path(), "", http.StatusOK)
		},
		SharedClaims: hostedSharedClaims, JSONError: s.hostedJSONError, InternalError: s.internalAPIError,
		PlatformOrganization: func() tracker.OrganizationID { return s.database.hostedOrganization },
	}
}

func creditCredential(value hubauth.Credential, err error) (apiCredential, error) {
	return nativeCredential(value), err
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

type aiCreditTransaction = credits.Transaction
type aiCreditView = credits.View
type includedAICredits credits.Included
type creditPageRequest credits.Page
type preparedConversationUsage = credits.PreparedUsage

type chatUsageSummary = usagereport.ChatSummary

func (c includedAICredits) remaining() int64 { return credits.Included(c).Remaining() }
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
		return credits.Plan{Name: plan.Name, AllowanceMicros: plan.IncludedAICreditMicros()}, err
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
			return credits.Plan{Name: plan.Name, AllowanceMicros: plan.IncludedAICreditMicros()}, err
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

func chatBillingWindow(now time.Time, subscription billing.Snapshot) usageWindow {
	return usageWindow(credits.BillingWindow(now, subscription))
}
func (d *database) chatUsageSummary(ctx context.Context, organization tracker.OrganizationID, window usageWindow, projects []string) (chatUsageSummary, error) {
	value, err := d.creditsService().UsageSummary(ctx, organization, credits.Window(window), projects)
	return chatUsageSummary{Range: usageWindow(value.Range), Input: value.Input, CachedInput: value.CachedInput, Output: value.Output, ReasoningOutput: value.ReasoningOutput, Tokens: value.Tokens, Turns: value.Turns, UnpricedTurns: value.UnpricedTurns, CostUSD: value.CostUSD}, fromFeatureError(err)
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
