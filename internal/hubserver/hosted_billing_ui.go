package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

type hostedBillingReport struct {
	ChargedAIMicros int64                          `json:"charged_ai_micros"`
	AICredits       *aiCreditView                  `json:"ai_credits,omitempty"`
	ChatUsage       chatUsageSummary               `json:"chat_usage"`
	OrganizationID  string                         `json:"organization_id"`
	State           hostedBillingState             `json:"state"`
	Entitlement     HostedEntitlement              `json:"entitlement"`
	ReconciledAt    string                         `json:"reconciled_at"`
	PendingEvents   int64                          `json:"pending_events"`
	Audit           []templates.HostedBillingAudit `json:"recent_audit"`
}

func (s *Service) hostedBillingReport(ctx context.Context, pages ...creditPageRequest) (hostedBillingReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	report := hostedBillingReport{OrganizationID: s.config.Hosted.OrganizationID, Audit: []templates.HostedBillingAudit{}}
	var err error
	report.State, err = s.database.readHostedBilling(ctx)
	if err != nil {
		return report, err
	}
	report.Entitlement, err = s.database.hostedPlanUsage(ctx, s.config.now())
	if err != nil {
		return report, err
	}
	report.ChatUsage, err = s.database.chatUsageSummary(ctx, s.database.hostedOrganization, chatBillingWindow(s.config.now(), report.State.Snapshot), nil)
	if err != nil {
		return report, err
	}
	if s.database.aiCreditMode != "" {
		page := creditPageRequest{From: report.ChatUsage.Range.From.Format(time.RFC3339), To: report.ChatUsage.Range.To.Format(time.RFC3339)}
		if len(pages) > 0 {
			page = pages[0]
		}
		report.AICredits, err = s.readAICredits(ctx, page)
		if err != nil {
			return report, err
		}
		window := report.ChatUsage.Range
		if err := s.database.reader.QueryRowContext(ctx, `SELECT coalesce(-sum(amount_micros),0) FROM ai_credit_transactions
 WHERE organization_id=? AND mode=? AND kind='usage' AND recorded_at>=? AND recorded_at<?`, s.database.hostedOrganization, s.database.aiCreditMode, window.From.UnixMicro(), window.To.UnixMicro()).Scan(&report.ChargedAIMicros); err != nil {
			return report, err
		}
	}
	var checked sql.NullString
	err = s.database.reader.QueryRowContext(ctx, "SELECT reconciled_at FROM hosted_billing_accounts WHERE organization_id=?", s.config.Hosted.OrganizationID).Scan(&checked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return report, err
	}
	report.ReconciledAt = checked.String
	if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM hosted_billing_events WHERE processed_at IS NULL").Scan(&report.PendingEvents); err != nil {
		return report, err
	}
	rows, err := s.database.reader.QueryContext(ctx, "SELECT actor_id,action,record_json,recorded_at FROM hosted_billing_audit WHERE organization_id=? ORDER BY id DESC LIMIT 50", s.config.Hosted.OrganizationID)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry templates.HostedBillingAudit
		var raw string
		if err := rows.Scan(&entry.Actor, &entry.Action, &raw, &entry.At); err != nil {
			return report, err
		}
		var summary struct {
			Status  string `json:"status"`
			PriceID string `json:"price_id"`
		}
		if err := json.Unmarshal([]byte(raw), &summary); err != nil {
			return report, err
		}
		entry.Summary = strings.TrimSpace(summary.Status + " " + summary.PriceID)
		report.Audit = append(report.Audit, entry)
	}
	return report, rows.Err()
}

func (s *Service) hostedBillingExport(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c.Request().Context(), c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "billing_exported", c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.hostedBillingReport(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) hostedBillingPage(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c.Request().Context(), c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "Billing requires an organization owner without support impersonation")
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "billing_viewed", c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.hostedBillingReport(c.Request().Context())
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Billing information is temporarily unavailable")
	}
	now := s.config.now()
	data := templates.HostedPageData{Mode: "billing", Title: "Organization billing", CanManage: true, CanManageOwnership: true}
	data.PlanName = report.Entitlement.Name
	data.PlanPrice = hostedPlanPrice(report.Entitlement.MonthlyUSDCents)
	for _, row := range [][2]string{{"concurrent_work", "concurrent work"}, {"unarchived_issues", "open issues"}} {
		name, label := row[0], row[1]
		limit, limited := report.Entitlement.Allowances[name]
		if !limited {
			continue
		}
		used := report.Entitlement.Usage[name]
		data.Allowances = append(data.Allowances, templates.HostedAllowanceRow{Label: label, Consumption: strconv.FormatInt(used, 10), Allowance: strconv.FormatInt(limit, 10), Remaining: strconv.FormatInt(max(limit-used, 0), 10), OverLimit: used > limit})
	}
	if credits := report.AICredits; credits != nil {
		data.AICredits = &templates.HostedAICredits{Returned: c.QueryParam("credits") == "returned", Balance: fmt.Sprintf("$%.6f USD", float64(credits.BalanceMicros)/1000000), Enabled: credits.AutoEnabled, Threshold: strconv.FormatInt(credits.ThresholdCents, 10), PriceID: credits.PriceID, Failure: credits.Failure, InFlight: credits.InFlight, CanAutoFund: credits.CanAutoFund}
		for _, pack := range credits.Packs {
			data.AICredits.Packs = append(data.AICredits.Packs, templates.HostedBillingPrice{ID: pack.PriceID, Label: fmt.Sprintf("%s · $%.2f USD", pack.Label, float64(pack.USDCents)/100)})
		}
		for _, item := range credits.History {
			summary := fmt.Sprintf("$%.6f USD", float64(item.AmountMicros)/1000000)
			if item.Kind == "complimentary" {
				summary += " · " + item.Actor + " · " + item.Reason
			}
			data.AICredits.History = append(data.AICredits.History, templates.HostedBillingAudit{Action: item.Kind, Summary: summary, At: item.At.Format(time.RFC3339)})
		}
	}
	data.BillingStatus, data.BillingMessage = hostedBillingMessage(report.State, now)
	if (report.State.Status == "free" || report.State.Status == "") && report.Entitlement.EffectiveBase.ID != "free" {
		if report.Entitlement.EffectiveBase.ID == "payment_pending" {
			data.BillingStatus, data.BillingMessage = "Payment pending", "Complete checkout to activate this organization's paid plan."
			data.PlanPrice = "Paid access starts after checkout"
		} else {
			data.BillingStatus, data.BillingMessage = "Complimentary access", "Your existing plan access is complimentary. No paid subscription is active."
		}
	}
	data.BillingAudit = report.Audit
	data.BillingCheckedAt = report.ReconciledAt
	data.ChatUsage = fmt.Sprintf("%d tokens · $%.6f USD · turns: %d", report.ChatUsage.Tokens, report.ChatUsage.CostUSD, report.ChatUsage.Turns)
	data.ChatUsagePeriod = report.ChatUsage.Range.From.Format("Jan 2, 2006") + " – " + report.ChatUsage.Range.To.Format("Jan 2, 2006")
	if report.ChatUsage.UnpricedTurns > 0 {
		data.ChatUsage += fmt.Sprintf(" · turns awaiting pricing: %d", report.ChatUsage.UnpricedTurns)
	}
	if report.PendingEvents > 0 {
		data.Notice = "A billing update is awaiting verification. Current access follows the last verified subscription deadline."
	}
	if c.QueryParam("checkout") != "" {
		data.Notice = "Checkout returned. Paid access starts only after Stripe confirms the subscription; this page does not activate a purchase."
	}
	for _, grant := range report.Entitlement.Grants {
		if grant.RevokedAt == nil && !grant.StartsAt.After(now) && (grant.ExpiresAt == nil || grant.ExpiresAt.After(now)) {
			message := "Complimentary access: " + strings.Join(grant.Scope, ", ")
			if grant.ExpiresAt != nil {
				message += " · expires " + grant.ExpiresAt.UTC().Format(time.RFC3339)
			}
			data.PlanGrants = append(data.PlanGrants, message)
		}
	}
	if cfg := s.hostedBillingConfig(); cfg != nil {
		data.BillingEnabled = true
		data.BillingCanPurchase = report.State.Snapshot.SubscriptionID == "" && report.State.Status != "multiple_subscriptions"
		data.BillingCheckoutPrice = c.QueryParam("checkout_price")
		for _, price := range cfg.Prices {
			data.BillingPrices = append(data.BillingPrices, templates.HostedBillingPrice{ID: price.PriceID, Label: s.hostedPriceLabel(c.Request().Context(), price)})
		}
	}
	return s.renderHosted(c, http.StatusOK, data)
}

func hostedBillingMessage(state hostedBillingState, now time.Time) (string, string) {
	if !state.AccessUntil.IsZero() && !state.AccessUntil.After(now) {
		return "Subscription access ended", "The base plan and valid complimentary grants apply. Update your subscription in the portal to recover paid allowances."
	}
	switch state.Status {
	case "active":
		if !state.PaidThrough.After(now) {
			return "Renewal verification grace", "Renewal has not been verified. Paid allowances continue until " + state.AccessUntil.Format(time.RFC3339) + "."
		}
		return "Subscribed", "Current paid period ends " + state.PaidThrough.Format(time.RFC3339) + "."
	case "canceling":
		return "Cancellation scheduled", "Subscription access continues until " + state.AccessUntil.Format(time.RFC3339) + ", then returns to the base plan and valid complimentary grants."
	case "trialing":
		return "Subscription trial", "Stripe trial access ends " + state.AccessUntil.Format(time.RFC3339) + ". Trials are separate from complimentary grants."
	case "grace":
		return "Payment failed · grace active", "Update payment in the portal. Paid allowances continue until " + state.GraceUntil.Format(time.RFC3339) + "; after that, new excess allocations pause."
	case "past_due", "payment_failed", "unpaid":
		return "Payment failed", "The base plan and valid complimentary grants apply. Update payment in the portal to recover paid access."
	case "refunded", "disputed":
		return "Payment review · " + state.Status, "Subscription allowances are suspended for a full current-invoice refund or unresolved/lost dispute. The base plan and complimentary grants remain available."
	case "canceled", "incomplete_expired":
		return "Canceled", "Your subscription has ended. Free access and valid complimentary grants remain available."
	case "incomplete":
		return "Payment pending", "Complete payment or authentication in Stripe. Paid access has not started."
	case "free", "":
		return "Free", "Free access requires no card or Stripe subscription."
	default:
		return "Subscription needs attention", "This subscription is paused or outside the approved plan configuration. Use the portal or contact your organization operator."
	}
}

type hostedBillingPriceView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type hostedBillingView struct {
	hostedBillingReport
	ComparisonPlans []hostedComparisonPlan   `json:"comparison_plans"`
	Prices          []hostedBillingPriceView `json:"prices"`
	CanCheckout     bool                     `json:"can_checkout"`
	CanManage       bool                     `json:"can_manage"`
	CheckoutPending bool                     `json:"checkout_pending"`
}

type hostedComparisonPlan struct {
	IncludedAICreditMicros *int64 `json:"included_ai_credit_micros"`
	HostedPlan
	PriceID string `json:"price_id"`
}

type hostedBillingUsageView struct {
	IncludedAICreditMicros *int64                 `json:"included_ai_credit_micros"`
	OrganizationID         string                 `json:"organization_id"`
	EffectiveBase          PlanReference          `json:"effective_base"`
	Name                   string                 `json:"name"`
	MonthlyUSDCents        *int64                 `json:"monthly_usd_cents"`
	Complimentary          bool                   `json:"complimentary"`
	ExpiresAt              *time.Time             `json:"expires_at"`
	RenewsAt               *time.Time             `json:"renews_at"`
	CancelsAt              *time.Time             `json:"cancels_at"`
	ChatUsage              chatUsageSummary       `json:"chat_usage"`
	ChargedAIMicros        int64                  `json:"charged_ai_micros"`
	AICredits              *aiCreditView          `json:"ai_credits,omitempty"`
	ComparisonPlans        []hostedComparisonPlan `json:"comparison_plans"`
	CanCheckout            bool                   `json:"can_checkout"`
	CanManage              bool                   `json:"can_manage"`
	CanBuyCredits          bool                   `json:"can_buy_credits"`
	CheckoutPending        bool                   `json:"checkout_pending"`
}

func (s *Service) hostedBillingJSON(c echo.Context) error {
	if c.QueryParam("view") == "usage" {
		return s.hostedBillingUsageJSON(c)
	}
	credential, err := s.hostedBillingOwner(c.Request().Context(), c)
	if err != nil {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Billing requires an organization owner without support impersonation", apiErrorResponse{Code: "forbidden", Message: "Billing requires an organization owner without support impersonation"})
	}
	ctx := c.Request().Context()
	if err := s.hostedAudit(ctx, credential.Hosted, "billing_viewed", c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	view, err := s.readBillingView(ctx)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "billing_unavailable", Message: "Billing information is temporarily unavailable"})
	}

	return c.JSON(http.StatusOK, view)
}

func (s *Service) hostedBillingUsageJSON(c echo.Context) error {
	cfg := s.hostedBillingConfig()
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Plan and usage require owner or admin access", apiErrorResponse{Code: "forbidden", Message: "Plan and usage require owner or admin access"})
	}
	view, err := s.readBillingView(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "billing_unavailable", Message: "Billing usage is temporarily unavailable"})
	}
	canBill := credential.HostedRole == "owner" && credential.Hosted.SupportActor == ""
	if view.AICredits != nil {
		view.AICredits.History = []aiCreditTransaction{}
		view.AICredits.CanAutoFund = false
	}
	plan, complimentary, expiresAt, err := s.billingUsagePlan(c.Request().Context(), view.Entitlement)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var renewsAt, cancelsAt *time.Time
	subscription := view.State.Snapshot
	if (subscription.Status == "active" || subscription.Status == "trialing") && !subscription.CancelAtPeriodEnd && subscription.CancelAt.IsZero() {
		if !subscription.PeriodEnd.IsZero() {
			renewsAt = &subscription.PeriodEnd
		}
	}
	if subscription.Status == "active" || subscription.Status == "trialing" {
		if !subscription.CancelAt.IsZero() {
			cancelsAt = &subscription.CancelAt
		} else if subscription.CancelAtPeriodEnd && !subscription.PeriodEnd.IsZero() {
			cancelsAt = &subscription.PeriodEnd
		}
	}
	return c.JSON(http.StatusOK, hostedBillingUsageView{
		OrganizationID: view.OrganizationID, EffectiveBase: plan.PlanReference, Name: plan.Name, MonthlyUSDCents: plan.MonthlyUSDCents, Complimentary: complimentary, ExpiresAt: expiresAt, RenewsAt: renewsAt, CancelsAt: cancelsAt,
		IncludedAICreditMicros: view.Entitlement.IncludedAICreditMicros,
		ChatUsage:              view.ChatUsage, ChargedAIMicros: view.ChargedAIMicros, AICredits: view.AICredits,
		ComparisonPlans: view.ComparisonPlans, CanCheckout: canBill && view.CanCheckout,
		CanManage:       canBill && view.CanManage,
		CanBuyCredits:   canBill && cfg != nil && !cfg.CheckoutDisabled && view.AICredits != nil && len(view.AICredits.Packs) > 0,
		CheckoutPending: view.CheckoutPending,
	})
}

func (s *Service) billingUsagePlan(ctx context.Context, entitlement HostedEntitlement) (HostedPlan, bool, *time.Time, error) {
	plan, err := s.database.readHostedPlan(ctx, s.database.reader, entitlement.EffectiveBase)
	if err != nil {
		return HostedPlan{}, false, nil, err
	}
	if plan.Name == "" {
		plan.Name = "Current plan"
	}
	complimentary := entitlement.Source != "subscription" && slices.Contains([]string{"pilot_free", "comp_team"}, plan.ID)
	var expiry *time.Time
	matched := false
	now := s.config.now()
	for _, grant := range entitlement.Grants {
		if grant.RevokedAt != nil || grant.StartsAt.After(now) || grant.ExpiresAt != nil && !grant.ExpiresAt.After(now) {
			continue
		}
		candidate, err := s.database.readHostedPlan(ctx, s.database.reader, grant.Plan)
		if err != nil {
			return HostedPlan{}, false, nil, err
		}
		if hostedCapacityPlan(grant.Plan) && candidate.Allowances["concurrent_work"] > plan.Allowances["concurrent_work"] {
			plan, complimentary, expiry, matched = candidate, true, grant.ExpiresAt, true
		} else if grant.Plan == plan.PlanReference && (entitlement.Source != "subscription" || matched) {
			if !matched || expiry != nil && (grant.ExpiresAt == nil || grant.ExpiresAt.After(*expiry)) {
				expiry = grant.ExpiresAt
			}
			complimentary, matched = true, true
		}
	}
	return plan, complimentary, expiry, nil
}

func (s *Service) hostedPriceLabel(ctx context.Context, price HostedBillingPrice) string {
	plan, err := s.database.readHostedPlan(ctx, s.database.reader, price.Plan)
	if err != nil || plan.MonthlyUSDCents == nil {
		return price.Label
	}
	return plan.Name + " · " + hostedPlanPrice(plan.MonthlyUSDCents)
}

func (s *Service) readBillingView(ctx context.Context, pages ...creditPageRequest) (hostedBillingView, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	report, err := s.hostedBillingReport(ctx, pages...)
	if err != nil {
		return hostedBillingView{}, err
	}

	view := hostedBillingView{hostedBillingReport: report, Prices: []hostedBillingPriceView{}}
	view.ComparisonPlans = []hostedComparisonPlan{}
	if runtimePlans := s.database.plans(); runtimePlans != nil {
		for _, tier := range hostedTiers() {
			if !tier.Offered || !slices.ContainsFunc(runtimePlans.Plans, func(plan HostedPlan) bool { return plan.PlanReference == tier.PlanReference }) {
				continue
			}
			plan, err := s.database.readHostedPlan(ctx, s.database.reader, tier.PlanReference)
			if err != nil {
				return hostedBillingView{}, err
			}
			comparison := hostedComparisonPlan{HostedPlan: plan, IncludedAICreditMicros: plan.IncludedAICreditMicros()}
			if cfg := s.hostedBillingConfig(); cfg != nil {
				for _, price := range cfg.Prices {
					if price.Plan == plan.PlanReference {
						comparison.PriceID = price.PriceID
						break
					}
				}
			}
			view.ComparisonPlans = append(view.ComparisonPlans, comparison)
		}
	}
	if cfg := s.hostedBillingConfig(); cfg != nil {
		for _, price := range cfg.Prices {
			view.Prices = append(view.Prices, hostedBillingPriceView{ID: price.PriceID, Label: s.hostedPriceLabel(ctx, price)})
		}
		view.CanCheckout = !cfg.CheckoutDisabled && report.State.Snapshot.SubscriptionID == "" && report.State.Status != "multiple_subscriptions"
		if _, err := s.database.hostedBillingBinding(ctx, cfg); err == nil {
			view.CanManage = true
		}
		var raw string
		if err := s.database.reader.QueryRowContext(ctx, "SELECT checkout_json FROM hosted_billing_accounts WHERE organization_id=?", s.config.Hosted.OrganizationID).Scan(&raw); err == nil {
			var checkout hostedCheckout
			view.CheckoutPending = json.Unmarshal([]byte(raw), &checkout) == nil && checkout.ExpiresAt.After(s.config.now())
		}
	}
	if view.Audit == nil {
		view.Audit = []templates.HostedBillingAudit{}
	}
	return view, nil
}
