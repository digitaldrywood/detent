package credits

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/usagecost"
)

type PreparedUsage struct {
	usage  Usage
	record billingstore.ConversationUsage
	cost   billingstore.CostObservation
}

func (s *Service) PrepareUsage(ctx context.Context, usage Usage) (PreparedUsage, error) {
	var prepared PreparedUsage
	if s.deps.Details == nil {
		return prepared, errors.New("billing detail storage is unavailable")
	}
	if err := s.deps.Repository.ReadTenant(ctx, usage.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := s.UsageRecord(ctx, r, usage)
		prepared.record = record
		return err
	}); err != nil {
		return prepared, err
	}
	usage.OccurredAt = prepared.record.OccurredAt
	prepared.usage = usage
	observation, err := UsageCostObservation(prepared.record)
	if err != nil {
		return prepared, err
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return prepared, err
	}
	ref, err := s.deps.Details.StoreDetail(ctx, string(usage.OrganizationID), raw)
	if err != nil {
		return prepared, err
	}
	if err := s.deps.ValidateDetail(ctx, s.deps.Details, usage.OrganizationID, ref, raw); err != nil {
		return prepared, err
	}
	prepared.cost = billingstore.CostObservation{OrganizationID: usage.OrganizationID, ProjectID: usage.ProjectID, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: observation.ObservedAt}
	return prepared, nil
}

func UsageCostObservation(record billingstore.ConversationUsage) (usagecost.Observation, error) {
	quantity := float64(record.Input) + float64(record.Output)
	end := record.OccurredAt.Add(time.Microsecond)
	observation := usagecost.Observation{Provider: record.Provider, ProviderAccount: "luna", ResourceID: record.TurnID, Bucket: "luna_api", Metric: record.Model, Model: record.Model, BillingMode: "metered", SourceID: "luna:" + record.TurnID, Revision: 1, From: record.OccurredAt, To: end, Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", Input: record.Input, CachedInput: record.CachedInput, Output: record.Output, Currency: "USD", Basis: "unknown", EvidenceSource: "conversation_usage", ObservedAt: end, FreshUntil: end, Coverage: "complete"}
	if record.CostUSD != nil {
		micros := math.Round(*record.CostUSD * 1e6)
		if math.IsNaN(micros) || math.IsInf(micros, 0) || micros < 0 || micros > 1e15 {
			return usagecost.Observation{}, billingstore.ErrInvalidAmount
		}
		amount := int64(micros)
		observation.AmountMicros, observation.EstimatedAmountMicros, observation.Basis = &amount, &amount, "estimated"
		if record.PriceID != nil {
			observation.EstimateSource = "conversation_prices:" + strconv.FormatInt(*record.PriceID, 10)
		}
	}
	return observation, nil
}
