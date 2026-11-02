package hubserver

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
)

type preparedConversationUsage struct {
	usage  ConversationUsage
	record billingstore.ConversationUsage
	cost   billingstore.CostObservation
}

func (d *database) prepareConversationUsagePostgres(ctx context.Context, usage ConversationUsage) (preparedConversationUsage, error) {
	var prepared preparedConversationUsage
	if d.billingAuditStore == nil {
		return prepared, errors.New("billing detail storage is unavailable")
	}
	if err := d.billingRepository.ReadTenant(ctx, usage.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := d.conversationUsageRecord(ctx, r, usage)
		prepared.record = record
		return err
	}); err != nil {
		return prepared, err
	}
	usage.OccurredAt = prepared.record.OccurredAt
	prepared.usage = usage
	observation, err := conversationUsageCostObservation(prepared.record)
	if err != nil {
		return prepared, err
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return prepared, err
	}
	ref, err := d.billingAuditStore.StoreDetail(ctx, string(usage.OrganizationID), raw)
	if err != nil {
		return prepared, err
	}
	if err := validateNativeDetailReference(ctx, d.billingAuditStore, usage.OrganizationID, ref, raw); err != nil {
		return prepared, err
	}
	prepared.cost = billingstore.CostObservation{OrganizationID: usage.OrganizationID, ProjectID: usage.ProjectID, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: observation.ObservedAt}
	return prepared, nil
}

func conversationUsageCostObservation(record billingstore.ConversationUsage) (costObservation, error) {
	quantity := float64(record.Input) + float64(record.Output)
	end := record.OccurredAt.Add(time.Microsecond)
	observation := costObservation{Provider: record.Provider, ProviderAccount: "luna", ResourceID: record.TurnID, Bucket: lunaCostBucket, Metric: record.Model, Model: record.Model, BillingMode: "metered", SourceID: "luna:" + record.TurnID, Revision: 1, From: record.OccurredAt, To: end, Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", Input: record.Input, CachedInput: record.CachedInput, Output: record.Output, Currency: "USD", Basis: "unknown", EvidenceSource: "conversation_usage", ObservedAt: end, FreshUntil: end, Coverage: "complete"}
	if record.CostUSD != nil {
		micros := math.Round(*record.CostUSD * 1e6)
		if math.IsNaN(micros) || math.IsInf(micros, 0) || micros < 0 || micros > 1e15 {
			return costObservation{}, billingstore.ErrInvalidAmount
		}
		amount := int64(micros)
		observation.AmountMicros, observation.EstimatedAmountMicros, observation.Basis = &amount, &amount, "estimated"
		if record.PriceID != nil {
			observation.EstimateSource = "conversation_prices:" + strconv.FormatInt(*record.PriceID, 10)
		}
	}
	return observation, nil
}
