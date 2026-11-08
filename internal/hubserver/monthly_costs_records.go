package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

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
	type conversationCostKey struct {
		project        tracker.ProjectID
		provider, turn string
	}
	recorded := map[conversationCostKey]bool{}
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
		if o.Bucket == lunaCostBucket && o.ProviderAccount == "luna" && o.SourceID == "luna:"+o.ResourceID {
			recorded[conversationCostKey{revision.ProjectID, o.Provider, o.ResourceID}] = true
		}
	}
	for _, u := range snapshot.Conversations {
		if recorded[conversationCostKey{u.ProjectID, u.Provider, u.TurnID}] {
			continue
		}
		o, err := conversationUsageCostObservation(u)
		if err != nil {
			return nil, nil, snapshot, err
		}
		observations = append(observations, attributedCostObservation{ProjectID: string(u.ProjectID), costObservation: o})
	}
	resources := []costResource{}
	for _, r := range snapshot.Resources {
		resources = append(resources, costResource{ProjectID: string(r.ProjectID), Provider: "fly_sprites", ProviderAccount: r.ProviderAccount, ResourceName: r.Name})
	}
	return observations, resources, snapshot, nil
}
