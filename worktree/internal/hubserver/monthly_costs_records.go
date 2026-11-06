package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *database) monthlyCostRecords(ctx context.Context, organization string, projects []string, window usageWindow) ([]attributedCostObservation, []costResource, billingstore.MonthlyCostSnapshot, error) {
	var snapshot billingstore.MonthlyCostSnapshot
	err := d.billingRepository.ReadTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(billingstore.MonthlyCostReader)
		if !ok {
			return errors.New("monthly cost reader unavailable")
		}
		var err error
		snapshot, err = reader.ReadMonthlyCosts(ctx, billingstore.MonthlyCostQuery{Projects: projects, From: window.From, To: window.To})
		return err
	})
	if err != nil {
		return nil, nil, snapshot, err
	}
	observations := []attributedCostObservation{}
	for _, revision := range snapshot.Revisions {
		if d.billingAuditStore == nil {
			return nil, nil, snapshot, errors.New("monthly cost archives unavailable")
		}
		ref := revision.Reference.ArchiveReference()
		if err := ref.Validate(organization); err != nil {
			return nil, nil, snapshot, err
		}
		raw, err := d.billingAuditStore.ReadDetail(ctx, organization, ref)
		if err != nil {
			return nil, nil, snapshot, err
		}
		var o costObservation
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, nil, snapshot, err
		}
		var identity []byte
		for _, value := range []string{o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costTimeLayout), o.To.UTC().Format(costTimeLayout)} {
			identity = strconv.AppendInt(identity, int64(len(value)), 10)
			identity = append(identity, ':')
			identity = append(identity, value...)
		}
		digest := sha256.Sum256(identity)
		if hex.EncodeToString(digest[:]) != revision.SourceKey || o.Revision != revision.Revision || o.Voided {
			return nil, nil, snapshot, errors.New("monthly cost archive identity mismatch")
		}
		observations = append(observations, attributedCostObservation{ProjectID: string(revision.ProjectID), ReceivedAt: revision.ReceivedAt, costObservation: o})
	}
	for _, u := range snapshot.Conversations {
		from := u.OccurredAt
		quantity := float64(u.Input) + float64(u.Output)
		o := costObservation{Provider: u.Provider, ProviderAccount: "luna", ResourceID: u.TurnID, Bucket: lunaCostBucket, Metric: u.Model, Model: u.Model, BillingMode: "metered", SourceID: "luna:" + u.TurnID, Revision: 1, From: from, To: from.Add(time.Microsecond), Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", Input: u.Input, CachedInput: u.CachedInput, Output: u.Output, Currency: "USD", Basis: "unknown", EvidenceSource: "conversation_usage", ObservedAt: from.Add(time.Microsecond), FreshUntil: from.Add(time.Microsecond), Coverage: "complete"}
		if u.CostUSD != nil {
			amount := int64(math.Round(*u.CostUSD * 1e6))
			o.AmountMicros, o.EstimatedAmountMicros, o.Basis = &amount, &amount, "estimated"
			if u.PriceID != nil {
				o.EstimateSource = "conversation_prices:" + strconv.FormatInt(*u.PriceID, 10)
			}
		}
		observations = append(observations, attributedCostObservation{ProjectID: string(u.ProjectID), costObservation: o})
	}
	resources := []costResource{}
	for _, r := range snapshot.Resources {
		resources = append(resources, costResource{ProjectID: string(r.ProjectID), Provider: "fly_sprites", ProviderAccount: r.ProviderAccount, ResourceName: r.Name})
	}
	return observations, resources, snapshot, nil
}
