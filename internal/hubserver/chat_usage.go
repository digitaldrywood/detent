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
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin conversation usage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := d.recordConversationUsage(ctx, tx, usage); err != nil {
		return err
	}
	return tx.Commit()
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
	if count == 1 && d.aiCreditMode != "" && usage.Provider == "openai" && cost != nil {
		micros := int64(math.Ceil(costUSD * 1000000 * d.aiCreditCostMultiplier))
		if _, err := tx.ExecContext(ctx, "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,?,?,?,'usage',?)", usage.OrganizationID, d.aiCreditMode, "usage:"+usage.TurnID, -micros, at.UnixMicro()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE ai_credit_accounts SET balance_micros=balance_micros-? WHERE organization_id=? AND mode=?", micros, usage.OrganizationID, d.aiCreditMode); err != nil {
			return err
		}
	}
	return nil
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
	query := `SELECT coalesce(sum(input),0),coalesce(sum(cached_input),0),coalesce(sum(output),0),coalesce(sum(reasoning_output),0),count(*),coalesce(sum(price_id IS NULL),0),coalesce(sum(cost_usd),0)
 FROM conversation_usage WHERE organization_id=? AND occurred_at>=? AND occurred_at<?`
	args := []any{organization, window.From.UnixMicro(), window.To.UnixMicro()}
	if projects != nil {
		encoded, err := marshalNative(projects)
		if err != nil {
			return summary, err
		}
		query += " AND project_id IN (SELECT value FROM json_each(?))"
		args = append(args, encoded)
	}
	err := d.db.QueryRowContext(ctx, query, args...).Scan(&summary.Input, &summary.CachedInput, &summary.Output, &summary.ReasoningOutput, &summary.Turns, &summary.UnpricedTurns, &summary.CostUSD)
	summary.Tokens = summary.Input + summary.Output
	return summary, err
}

func (s *Service) chatUsageRows(ctx context.Context, window usageWindow, projects []string) ([]usageRow, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	encoded, err := marshalNative(projects)
	if err != nil {
		return nil, err
	}
	result, err := s.database.db.QueryContext(ctx, `SELECT turn_id,occurred_at,provider,model,input,cached_input,output,coalesce(cost_usd,0),coalesce(cache_savings_usd,0) FROM conversation_usage WHERE organization_id=? AND occurred_at>=? AND occurred_at<? AND project_id IN (SELECT value FROM json_each(?)) ORDER BY occurred_at,turn_id`, s.config.Hosted.OrganizationID, window.From.UnixMicro(), window.To.UnixMicro(), encoded)
	if err != nil {
		return nil, err
	}
	defer func() { _ = result.Close() }()
	var rows []usageRow
	for result.Next() {
		row := usageRow{Currency: "USD", Chat: true}
		var at int64
		if err := result.Scan(&row.AttemptID, &at, &row.Provider, &row.Model, &row.Input, &row.CachedInput, &row.Output, &row.Cost, &row.CacheSavings); err != nil {
			return nil, err
		}
		row.AttemptID = "chat:" + row.AttemptID
		row.Period = time.UnixMicro(at).UTC()
		rows = append(rows, row)
	}
	return rows, errors.Join(result.Err(), result.Close())
}
