package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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

func chatUsageCost(price UsagePrice, tokens runner.AgentTokenCounts) float64 {
	return (float64(tokens.InputTokens-tokens.CachedInputTokens)*price.Input + float64(tokens.CachedInputTokens)*price.CachedInput + float64(tokens.OutputTokens)*price.Output) / tokensPerPriceUnit
}

func (d *database) RecordConversationUsage(ctx context.Context, usage ConversationUsage) error {
	if d.billingRepository == nil {
		return errors.New("billing repository is unavailable")
	}
	return d.recordConversationUsagePostgres(ctx, usage)
}

func (d *database) recordConversationUsage(ctx context.Context, tx *sql.Tx, usage ConversationUsage) error {
	tokens := usage.Tokens
	usage.Model = strings.TrimSpace(usage.Model)
	if usage.Model == "" {
		usage.Model = "unknown"
	}
	if tokens.InputTokens < 0 || tokens.CachedInputTokens < 0 || tokens.CachedInputTokens > tokens.InputTokens || tokens.OutputTokens < 0 || tokens.ReasoningOutputTokens < 0 || tokens.ReasoningOutputTokens > tokens.OutputTokens {
		return errors.New("invalid conversation token counts")
	}
	at := usage.OccurredAt
	if at.IsZero() {
		at = d.now()
	}
	var matches int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM conversations WHERE id=? AND organization_id=? AND project_id=?", usage.ConversationID, usage.OrganizationID, usage.ProjectID).Scan(&matches); err != nil {
		return err
	}
	if matches != 1 || usage.TurnID == "" || usage.Provider == "" {
		return errors.New("invalid conversation usage attribution")
	}
	var priceID int64
	var price UsagePrice
	err := tx.QueryRowContext(ctx, `SELECT id,input,cached_input,output FROM conversation_prices WHERE provider=? AND model=? AND effective_at<=? ORDER BY effective_at DESC LIMIT 1`, usage.Provider, usage.Model, at.UnixMicro()).Scan(&priceID, &price.Input, &price.CachedInput, &price.Output)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read conversation price: %w", err)
	}
	var pricedID, cost, saving any
	var costUSD float64
	if err == nil {
		costUSD = chatUsageCost(price, tokens)
		pricedID, cost = priceID, costUSD
		saving = float64(tokens.CachedInputTokens) * max(0, price.Input-price.CachedInput) / tokensPerPriceUnit
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO conversation_usage(organization_id,project_id,conversation_id,turn_id,provider,model,occurred_at,input,cached_input,output,reasoning_output,outcome,price_id,cost_usd,cache_savings_usd)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(organization_id,turn_id) DO NOTHING`, usage.OrganizationID, usage.ProjectID, usage.ConversationID, usage.TurnID, usage.Provider, usage.Model, at.UnixMicro(), tokens.InputTokens, tokens.CachedInputTokens, tokens.OutputTokens, tokens.ReasoningOutputTokens, usage.Outcome, pricedID, cost, saving)
	if err != nil {
		return fmt.Errorf("record conversation usage: %w", err)
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 && d.billingRepository != nil && d.aiCreditMode != "" && usage.Provider == "openai" {
		var priorCost sql.NullFloat64
		if err := tx.QueryRowContext(ctx, `SELECT cost_usd FROM conversation_usage WHERE organization_id=? AND turn_id=?`, usage.OrganizationID, usage.TurnID).Scan(&priorCost); err != nil {
			return err
		}
		if priorCost.Valid {
			costUSD = priorCost.Float64
			cost = priorCost.Float64
		} else {
			cost = nil
		}
	}
	if d.billingRepository != nil && d.aiCreditMode != "" && usage.Provider == "openai" && cost != nil {
		micros := int64(math.Ceil(costUSD * 1000000 * d.aiCreditCostMultiplier))
		return d.recordConversationCredit(ctx, usage.OrganizationID, usage.TurnID, at, micros)
	}
	if count == 1 && d.aiCreditMode != "" && usage.Provider == "openai" && cost != nil {
		micros := int64(math.Ceil(costUSD * 1000000 * d.aiCreditCostMultiplier))
		included, err := d.spendIncludedAICredits(ctx, tx, usage.OrganizationID, at, micros)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,included_amount_micros,kind,recorded_at) VALUES(?,?,?,?,?,'usage',?)", usage.OrganizationID, d.aiCreditMode, "usage:"+usage.TurnID, -micros, included, at.UnixMicro()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE ai_credit_accounts SET balance_micros=balance_micros-? WHERE organization_id=? AND mode=?", micros-included, usage.OrganizationID, d.aiCreditMode); err != nil {
			return err
		}
	}
	return nil
}

func (d *database) recordConversationCredit(ctx context.Context, organization tracker.OrganizationID, turnID string, at time.Time, micros int64) error {
	if micros < 0 {
		return errors.New("invalid conversation credit amount")
	}
	const sourcePrefix = "usage:"
	source := sourcePrefix + turnID
	return d.billingRepository.WriteTenant(ctx, organization, corestore.Serving, func(writer tenantstore.Writer) error {
		included, err := d.includedAICreditsFromReader(ctx, writer, at)
		if err != nil {
			return err
		}
		period := billingstore.CreditPeriod{OrganizationID: organization, Mode: d.aiCreditMode, StartsAt: included.PeriodStart, EndsAt: included.PeriodEnd, AllowanceMicros: included.AllowanceMicros, UsedMicros: included.UsedMicros}
		row := billingstore.CreditTransaction{OrganizationID: organization, Mode: d.aiCreditMode, Source: source, AmountMicros: -micros, Kind: "usage", RecordedAt: at.UTC()}
		_, err = writer.PostBillingUsageCredit(ctx, period, row, micros)
		return err
	})
}

func chatBillingWindow(now time.Time, subscription billing.Snapshot) usageWindow {
	now = now.UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	if !subscription.PeriodStart.IsZero() && !subscription.PeriodStart.After(now) && subscription.PeriodEnd.After(now) {
		from, to = subscription.PeriodStart.UTC(), subscription.PeriodEnd.UTC()
	}
	return usageWindow{From: from, To: to}
}

func (d *database) chatUsageSummary(ctx context.Context, organization tracker.OrganizationID, window usageWindow, projects []string) (chatUsageSummary, error) {
	summary := chatUsageSummary{Range: window}
	if d.billingRepository == nil {
		return summary, errors.New("billing repository is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	err := d.billingRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.ConversationUsageSummary(ctx, window.From, window.To, projects)
		if err != nil {
			return err
		}
		summary.Input, summary.CachedInput, summary.Output, summary.ReasoningOutput = row.Input, row.CachedInput, row.Output, row.ReasoningOutput
		summary.Turns, summary.UnpricedTurns, summary.CostUSD = row.Turns, row.UnpricedTurns, row.CostUSD
		summary.Tokens = row.Input + row.Output
		return nil
	})
	return summary, err
}

func (s *Service) chatUsageRows(ctx context.Context, window usageWindow, projects []string) ([]usageRow, error) {
	if s.database.billingRepository == nil {
		return nil, errors.New("billing repository is unavailable")
	}
	if len(projects) == 0 {
		return []usageRow{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var rows []usageRow
	err := s.database.billingRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(r tenantstore.Reader) error {
		buckets, err := r.ConversationUsageBuckets(ctx, window.From, window.To, projects, window.Hourly)
		if err != nil {
			return err
		}
		for _, bucket := range buckets {
			rows = append(rows, usageRow{Chat: true, ChatSessions: bucket.Turns, Period: bucket.Period, Provider: bucket.Provider, Model: bucket.Model, Input: bucket.Input, CachedInput: bucket.CachedInput, Output: bucket.Output, Cost: bucket.CostUSD, CacheSavings: bucket.CacheSavingsUSD, Currency: "USD"})
		}
		return nil
	})
	return rows, err
}
