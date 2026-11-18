package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func costTestObservation(metric string, from, to time.Time) costObservation {
	quantity, rate := 2.0, int64(10_000)
	unit := "GB-hour"
	if metric == "cpu" {
		unit = "CPU-hour"
	}
	return costObservation{Provider: "fly_sprites", ProviderAccount: "customer", ResourceID: "sprite-immutable-id", ResourceName: "worker", Bucket: spriteCostBucket, Metric: metric, SourceID: "meter-" + metric, Revision: 1, From: from, To: to, Quantity: &quantity, Unit: unit, QuantityBasis: "measured", Currency: "USD", Basis: "estimated", UnitPriceMicros: &rate, RateSource: "https://fly.io/sprites/", RateEffectiveAt: from.AddDate(0, -1, 0), EvidenceSource: "measurement-batch-1", ObservedAt: to, FreshUntil: to.AddDate(1, 0, 0), Coverage: "complete"}
}

func recordPostgresTestCost(t *testing.T, d *database, organization, project string, observation costObservation, now time.Time) (bool, error) {
	t.Helper()
	if err := normalizeCostObservation(&observation, now); err != nil {
		return false, err
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return false, err
	}
	ref, err := d.billingAuditStore.StoreDetail(t.Context(), organization, raw)
	if err != nil {
		return false, err
	}
	var changed bool
	err = d.billingRepository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		changed, err = w.RecordBillingCost(t.Context(), billingstore.CostObservation{OrganizationID: tracker.OrganizationID(organization), ProjectID: tracker.ProjectID(project), Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now})
		return err
	})
	return changed, err
}
