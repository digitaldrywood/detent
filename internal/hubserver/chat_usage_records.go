package hubserver

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) recordConversationUsagePostgres(ctx context.Context, usage ConversationUsage) error {
	return d.billingRepository.WriteTenant(ctx, usage.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return d.writeConversationUsagePostgres(ctx, w, usage)
	})
}

func (d *database) writeConversationUsagePostgres(ctx context.Context, w tenantstore.Writer, usage ConversationUsage) error {
	at := usage.OccurredAt
	previous, err := w.ConversationUsage(ctx, usage.TurnID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if at.IsZero() {
		if err == nil {
			at = previous.OccurredAt
		} else {
			at = d.now()
		}
	}
	at = time.UnixMicro(at.UnixMicro()).UTC()
	model := strings.TrimSpace(usage.Model)
	if model == "" {
		model = "unknown"
	}
	record := billingstore.ConversationUsage{OrganizationID: usage.OrganizationID, ProjectID: usage.ProjectID, ConversationID: usage.ConversationID, TurnID: usage.TurnID, Provider: usage.Provider, Model: model, OccurredAt: at, Input: usage.Tokens.InputTokens, CachedInput: usage.Tokens.CachedInputTokens, Output: usage.Tokens.OutputTokens, ReasoningOutput: usage.Tokens.ReasoningOutputTokens, Outcome: string(usage.Outcome)}
	if err == nil {
		record.PriceID, record.CostUSD, record.CacheSavingsUSD = previous.PriceID, previous.CostUSD, previous.CacheSavingsUSD
	} else {
		price, err := w.ConversationPrice(ctx, usage.Provider, model, at)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			cost := chatUsageCost(UsagePrice{Input: price.Input, CachedInput: price.CachedInput, Output: price.Output}, usage.Tokens)
			saving := float64(usage.Tokens.CachedInputTokens) * max(0, price.Input-price.CachedInput) / tokensPerPriceUnit
			record.PriceID, record.CostUSD, record.CacheSavingsUSD = &price.ID, &cost, &saving
		}
	}
	created, err := w.RecordConversationUsage(ctx, record)
	if err != nil || !created {
		return err
	}
	if d.aiCreditMode == "" || usage.Provider != "openai" || record.CostUSD == nil {
		return nil
	}
	amount := math.Ceil(*record.CostUSD * 1e6 * d.aiCreditCostMultiplier)
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || amount >= float64(math.MaxInt64) {
		return billingstore.ErrInvalidAmount
	}
	if amount == 0 {
		return nil
	}
	included, err := d.includedAICreditsFromReader(ctx, w, at)
	if err != nil {
		return err
	}
	if included.PeriodStart.IsZero() {
		included.PeriodStart = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		included.PeriodEnd = included.PeriodStart.AddDate(0, 1, 0)
	}
	period := billingstore.CreditPeriod{OrganizationID: usage.OrganizationID, Mode: d.aiCreditMode, StartsAt: included.PeriodStart, EndsAt: included.PeriodEnd, AllowanceMicros: included.AllowanceMicros, UsedMicros: included.UsedMicros}
	row := billingstore.CreditTransaction{OrganizationID: usage.OrganizationID, Mode: d.aiCreditMode, Source: "usage:" + usage.TurnID, AmountMicros: -int64(amount), Kind: "usage", RecordedAt: at}
	_, err = w.PostBillingUsageCredit(ctx, period, row, int64(amount))
	return err
}
