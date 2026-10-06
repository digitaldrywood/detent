package hubserver

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"time"
)

func readLunaCostObservations(ctx context.Context, query nativeQueryer, organization string, projects []string, window usageWindow) ([]attributedCostObservation, error) {
	encoded, err := json.Marshal(projects)
	if err != nil {
		return nil, err
	}
	rows, err := query.QueryContext(ctx, `SELECT project_id,turn_id,provider,model,occurred_at,input,cached_input,output,price_id,cost_usd FROM conversation_usage WHERE organization_id=? AND occurred_at>=? AND occurred_at<? AND (? OR project_id IN (SELECT value FROM json_each(?))) ORDER BY project_id,occurred_at,turn_id`, organization, window.From.UnixMicro(), window.To.UnixMicro(), projects == nil, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := []attributedCostObservation{}
	for rows.Next() {
		var project, turn, provider, model string
		var at, input, cached, output int64
		var priceID *int64
		var cost *float64
		if err := rows.Scan(&project, &turn, &provider, &model, &at, &input, &cached, &output, &priceID, &cost); err != nil {
			return nil, err
		}
		from := time.UnixMicro(at).UTC()
		quantity := float64(input) + float64(output)
		o := costObservation{Provider: provider, ProviderAccount: "luna", ResourceID: turn, Bucket: lunaCostBucket, Metric: model, Model: model, BillingMode: "metered", SourceID: "luna:" + turn, Revision: 1, From: from, To: from.Add(time.Microsecond), Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", Input: input, CachedInput: cached, Output: output, Currency: "USD", Basis: "unknown", EvidenceSource: "conversation_usage", ObservedAt: from.Add(time.Microsecond), FreshUntil: from.Add(time.Microsecond), Coverage: "complete"}
		if cost != nil {
			amount := int64(math.Round(*cost * 1e6))
			o.AmountMicros, o.EstimatedAmountMicros, o.Basis = &amount, &amount, "estimated"
			if priceID != nil {
				o.EstimateSource = "conversation_prices:" + strconv.FormatInt(*priceID, 10)
			}
		}
		observations = append(observations, attributedCostObservation{ProjectID: project, costObservation: o})
	}
	return observations, rows.Err()
}
