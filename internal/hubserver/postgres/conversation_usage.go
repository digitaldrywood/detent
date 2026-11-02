package postgres

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func (s *session) ConversationPrice(ctx context.Context, provider, model string, at time.Time) (billingstore.ConversationPrice, error) {
	var price billingstore.ConversationPrice
	var effective int64
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,id,provider,model,effective_at,input,cached_input,output FROM conversation_prices WHERE organization_id=$1 AND provider=$2 AND model=$3 AND effective_at<=$4 ORDER BY effective_at DESC LIMIT 1`, s.organization, provider, model, at.UnixMicro()).Scan(&price.OrganizationID, &price.ID, &price.Provider, &price.Model, &effective, &price.Input, &price.CachedInput, &price.Output)
	price.EffectiveAt = time.UnixMicro(effective).UTC()
	return price, mapError(ctx, err)
}

func (s *session) CreateConversationPrice(ctx context.Context, price billingstore.ConversationPrice) error {
	if err := s.checkOrganization(price.OrganizationID); err != nil {
		return err
	}
	if price.ID <= 0 || price.Provider == "" || price.Model == "" || price.EffectiveAt.IsZero() {
		return billingstore.ErrInvalidUsage
	}
	for _, value := range []float64{price.Input, price.CachedInput, price.Output} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return billingstore.ErrInvalidUsage
		}
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO conversation_prices(organization_id,id,provider,model,effective_at,input,cached_input,output) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, s.organization, price.ID, price.Provider, price.Model, price.EffectiveAt.UnixMicro(), price.Input, price.CachedInput, price.Output)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil || count == 1 {
		return err
	}
	previous, err := s.ConversationPrice(ctx, price.Provider, price.Model, price.EffectiveAt)
	if err != nil {
		return err
	}
	price.EffectiveAt = time.UnixMicro(price.EffectiveAt.UnixMicro()).UTC()
	if previous != price {
		return mutation.ErrConflict
	}
	return nil
}

func (s *session) ConversationUsage(ctx context.Context, turn string) (billingstore.ConversationUsage, error) {
	var usage billingstore.ConversationUsage
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT organization_id,project_id,conversation_id,turn_id,provider,model,occurred_at,input,cached_input,output,reasoning_output,outcome,price_id,cost_usd,cache_savings_usd FROM conversation_usage WHERE organization_id=$1 AND turn_id=$2`, s.organization, turn).Scan(&usage.OrganizationID, &usage.ProjectID, &usage.ConversationID, &usage.TurnID, &usage.Provider, &usage.Model, &at, &usage.Input, &usage.CachedInput, &usage.Output, &usage.ReasoningOutput, &usage.Outcome, &usage.PriceID, &usage.CostUSD, &usage.CacheSavingsUSD)
	usage.OccurredAt = time.UnixMicro(at).UTC()
	return usage, mapError(ctx, err)
}

func (s *session) RecordConversationUsage(ctx context.Context, usage billingstore.ConversationUsage) (bool, error) {
	if err := s.checkOrganization(usage.OrganizationID); err != nil {
		return false, err
	}
	if usage.ProjectID == "" || usage.ConversationID == "" || usage.TurnID == "" || usage.Provider == "" || usage.Model == "" || usage.OccurredAt.IsZero() || usage.Input < 0 || usage.CachedInput < 0 || usage.CachedInput > usage.Input || usage.Output < 0 || usage.ReasoningOutput < 0 || usage.ReasoningOutput > usage.Output {
		return false, billingstore.ErrInvalidUsage
	}
	if (usage.PriceID == nil) != (usage.CostUSD == nil) || (usage.PriceID == nil) != (usage.CacheSavingsUSD == nil) {
		return false, billingstore.ErrInvalidUsage
	}
	for _, value := range []*float64{usage.CostUSD, usage.CacheSavingsUSD} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return false, billingstore.ErrInvalidUsage
		}
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE organization_id=$1 AND project_id=$2 AND id=$3)`, s.organization, usage.ProjectID, usage.ConversationID).Scan(&exists); err != nil {
		return false, mapError(ctx, err)
	}
	if !exists {
		return false, errors.New("invalid conversation usage attribution")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO conversation_usage(organization_id,project_id,conversation_id,turn_id,provider,model,occurred_at,input,cached_input,output,reasoning_output,outcome,price_id,cost_usd,cache_savings_usd) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT(organization_id,turn_id) DO NOTHING`, s.organization, usage.ProjectID, usage.ConversationID, usage.TurnID, usage.Provider, usage.Model, usage.OccurredAt.UnixMicro(), usage.Input, usage.CachedInput, usage.Output, usage.ReasoningOutput, usage.Outcome, usage.PriceID, usage.CostUSD, usage.CacheSavingsUSD)
	if err != nil {
		return false, mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil || count == 1 {
		return count == 1, err
	}
	previous, err := s.ConversationUsage(ctx, usage.TurnID)
	if err != nil {
		return false, err
	}
	usage.OccurredAt = time.UnixMicro(usage.OccurredAt.UnixMicro()).UTC()
	if !reflect.DeepEqual(previous, usage) {
		return false, mutation.ErrConflict
	}
	return false, nil
}

func (s *session) ConversationUsageSummary(ctx context.Context, from, to time.Time, projects []string) (billingstore.ConversationUsageSummary, error) {
	var result billingstore.ConversationUsageSummary
	if from.IsZero() || !to.After(from) || len(projects) > 200 {
		return result, corestore.ErrInvalidPage
	}
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(sum(input),0),coalesce(sum(cached_input),0),coalesce(sum(output),0),coalesce(sum(reasoning_output),0),count(*),count(*) FILTER (WHERE price_id IS NULL),coalesce(sum(cost_usd),0) FROM conversation_usage WHERE organization_id=$1 AND occurred_at >= $2 AND occurred_at < $3 AND ($4 OR project_id=ANY($5::text[]))`, s.organization, from.UnixMicro(), to.UnixMicro(), projects == nil, pq.Array(projects)).Scan(&result.Input, &result.CachedInput, &result.Output, &result.ReasoningOutput, &result.Turns, &result.UnpricedTurns, &result.CostUSD)
	return result, mapError(ctx, err)
}

func (s *session) ConversationUsageBuckets(ctx context.Context, from, to time.Time, projects []string, hourly bool) ([]billingstore.ConversationUsageBucket, error) {
	if from.IsZero() || !to.After(from) || len(projects) > 200 {
		return nil, corestore.ErrInvalidPage
	}
	width := int64(24 * time.Hour / time.Microsecond)
	if hourly {
		width = int64(time.Hour / time.Microsecond)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT floor(occurred_at::numeric/$6)::bigint*$6 AS period,provider,model,count(*),coalesce(sum(input),0),coalesce(sum(cached_input),0),coalesce(sum(output),0),coalesce(sum(cost_usd),0),coalesce(sum(cache_savings_usd),0) FROM conversation_usage WHERE organization_id=$1 AND occurred_at >= $2 AND occurred_at < $3 AND ($4 OR project_id=ANY($5::text[])) GROUP BY period,provider,model ORDER BY period,provider,model`, s.organization, from.UnixMicro(), to.UnixMicro(), projects == nil, pq.Array(projects), width)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []billingstore.ConversationUsageBucket{}
	for rows.Next() {
		var row billingstore.ConversationUsageBucket
		var period int64
		if err := rows.Scan(&period, &row.Provider, &row.Model, &row.Turns, &row.Input, &row.CachedInput, &row.Output, &row.CostUSD, &row.CacheSavingsUSD); err != nil {
			return nil, mapError(ctx, err)
		}
		row.Period = time.UnixMicro(period).UTC()
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
