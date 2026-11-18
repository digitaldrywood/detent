package hostedplans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runtimeSnapshotKey struct {
	hubmaintenance.Tenant
	Revision string
}

type HostedRuntimeSnapshots struct {
	repository tenantstore.Repository
	objects    attachment.DetailStore
	mu         sync.Mutex
	cache      map[runtimeSnapshotKey][]byte
	loads      singleflight.Group
}

func NewHostedRuntimeSnapshots(repository tenantstore.Repository, objects attachment.DetailStore) (*HostedRuntimeSnapshots, error) {
	if repository == nil || objects == nil {
		return nil, errors.New("runtime snapshots require tenant records and immutable object storage")
	}
	return &HostedRuntimeSnapshots{repository: repository, objects: objects, cache: make(map[runtimeSnapshotKey][]byte)}, nil
}

func (s *HostedRuntimeSnapshots) tenantDatabase(tenant hubmaintenance.Tenant) (*Service, error) {
	if tenant.Organization == "" || len(tenant.Organization) > 128 || tenant.Generation <= 0 {
		return nil, hubapi.PublicInvalid("Invalid hosted runtime scope or payload")
	}
	return New(Deps{Repository: s.repository, Details: s.objects, Organization: tracker.OrganizationID(tenant.Organization), Now: time.Now}), nil
}

func (s *HostedRuntimeSnapshots) Load(ctx context.Context, tenant hubmaintenance.Tenant) (HostedRuntimeUpdate, error) {
	d, err := s.tenantDatabase(tenant)
	if err != nil {
		return HostedRuntimeUpdate{}, err
	}
	var revision billingstore.RuntimeRevision
	validated := false
	if err := d.ReadBillingTenant(ctx, func(r tenantstore.Reader) error {
		var err error
		revision, err = r.CurrentRuntime(ctx)
		if err != nil {
			return err
		}
		s.mu.Lock()
		raw := s.cache[runtimeSnapshotKey{Tenant: tenant, Revision: revision.ID}]
		s.mu.Unlock()
		if raw == nil {
			return nil
		}
		var update HostedRuntimeUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		if err := d.validateConfiguredPlanReferences(ctx, r, *update.Plans); err != nil {
			return err
		}
		validated = true
		return nil
	}); err != nil {
		return HostedRuntimeUpdate{}, err
	}
	key := runtimeSnapshotKey{Tenant: tenant, Revision: revision.ID}
	flight, err := json.Marshal(key)
	if err != nil {
		return HostedRuntimeUpdate{}, err
	}
	value, err, _ := s.loads.Do(string(flight), func() (any, error) {
		s.mu.Lock()
		raw := s.cache[key]
		s.mu.Unlock()
		if raw == nil {
			var err error
			raw, err = s.objects.ReadDetail(ctx, tenant.Organization, revision.Reference.ArchiveReference())
			if err != nil {
				return nil, err
			}
		}
		if len(raw) > 1<<20 {
			return nil, hubapi.PublicInvalid("Invalid hosted runtime scope or payload")
		}
		var update HostedRuntimeUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			return nil, err
		}
		update.Reason = "published runtime snapshot"
		update.StageID = revision.ID
		if err := ValidateHostedRuntimeUpdate(update); err != nil {
			return nil, err
		}
		if !validated {
			if err := d.ReadBillingTenant(ctx, func(r tenantstore.Reader) error {
				current, err := r.CurrentRuntime(ctx)
				if err != nil {
					return err
				}
				if current.ID != revision.ID {
					return corestore.ErrRevisionConflict
				}
				return d.validateConfiguredPlanReferences(ctx, r, *update.Plans)
			}); err != nil {
				return nil, err
			}
		}
		s.mu.Lock()
		if len(s.cache) == 128 {
			for evicted := range s.cache {
				delete(s.cache, evicted)
				break
			}
		}
		s.cache[key] = slices.Clone(raw)
		s.mu.Unlock()
		return json.Marshal(update)
	})
	if err != nil {
		return HostedRuntimeUpdate{}, err
	}
	raw, ok := value.([]byte)
	if !ok {
		return HostedRuntimeUpdate{}, errors.New("runtime snapshot load returned an invalid payload")
	}
	var update HostedRuntimeUpdate
	err = json.Unmarshal(raw, &update)
	return update, err
}

func (s *HostedRuntimeSnapshots) Adopt(ctx context.Context, tenant hubmaintenance.Tenant, expected string, update HostedRuntimeUpdate) error {
	d, err := s.tenantDatabase(tenant)
	if err != nil {
		return err
	}
	if _, err := uuid.Parse(update.StageID); err != nil || update.ValidateOnly {
		return hubapi.PublicInvalid("Invalid hosted runtime scope or payload")
	}
	if err := ValidateHostedRuntimeUpdate(update); err != nil {
		return err
	}
	return d.ApplyRuntimeRecordsAtRevision(ctx, *update.Plans, update.Prices, "configuration", update.Reason, update.StageID, false, hostedRuntimeReplacePlans, &expected)
}

func (d *Service) ArchiveRuntime(ctx context.Context, plans HostedPlansConfig, prices []HostedBillingPrice, id string, published bool) (billingstore.RuntimeRevision, error) {
	if id == "" {
		id = uuid.NewString()
	}
	raw, err := json.Marshal(HostedRuntimeUpdate{Plans: &plans, Prices: prices})
	if err != nil {
		return billingstore.RuntimeRevision{}, err
	}
	ref, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
	return billingstore.RuntimeRevision{ID: id, Published: published, Reference: billingstore.AuditReferenceFromArchive(ref)}, err
}

func (d *Service) RefreshRuntime(ctx context.Context) error {
	if d.billingRepository == nil {
		return nil
	}
	current := d.hostedRuntime.Load()
	var revision billingstore.RuntimeRevision
	err := d.ReadBillingTenant(ctx, func(r tenantstore.Reader) error { var err error; revision, err = r.CurrentRuntime(ctx); return err })
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != nil && current.Revision == revision.ID {
		return nil
	}
	_, err, _ = d.runtimeLoads.Do(revision.ID, func() (any, error) {
		if d.hostedRuntime.Load() != current {
			return struct{}{}, nil
		}
		raw, err := d.billingAuditStore.ReadDetail(ctx, string(d.hostedOrganization), revision.Reference.ArchiveReference())
		if err != nil {
			return nil, err
		}
		var update HostedRuntimeUpdate
		if err := json.Unmarshal(raw, &update); err != nil {
			return nil, err
		}
		if update.Plans == nil {
			return nil, hubapi.WorkItemStorageUnavailable()
		}
		update.Reason = "published runtime snapshot"
		if err := ValidateHostedRuntimeUpdate(update); err != nil {
			return nil, err
		}
		if err := d.ApplyPublishedPlans(ctx, revision, *update.Plans); err != nil {
			return nil, err
		}
		state := &RuntimeConfig{Revision: revision.ID, Plans: *update.Plans}
		if d.runtimeBilling != nil {
			next := *d.runtimeBilling
			next.Prices = update.Prices
			state.Billing = &next
		}
		d.hostedRuntime.CompareAndSwap(current, state)
		return struct{}{}, nil
	})
	return err
}

func (d *Service) ApplyPublishedPlans(ctx context.Context, revision billingstore.RuntimeRevision, plans HostedPlansConfig) error {
	return d.WriteBillingTenant(ctx, func(w tenantstore.Writer) error {
		current, err := w.CurrentRuntime(ctx)
		if err != nil {
			return err
		}
		if current.ID != revision.ID {
			return corestore.ErrRevisionConflict
		}
		if err := d.validateConfiguredPlanReferences(ctx, w, plans); err != nil {
			return err
		}
		return d.replacePublishedPlans(ctx, w, revision, plans)
	})
}

func (d *Service) replacePublishedPlans(ctx context.Context, w tenantstore.Writer, revision billingstore.RuntimeRevision, plans HostedPlansConfig) error {
	for _, configured := range plans.Plans {
		configured = NormalizePlan(configured)
		ref := billingstore.PlanReference{ID: configured.ID, Version: configured.Version}
		stored, err := w.BillingPlan(ctx, ref)
		if err != nil {
			return err
		}
		if stored.RuntimeID == revision.ID {
			continue
		}
		existing := NormalizePlan(HostedPlan{PlanReference: configured.PlanReference, Name: stored.Name, MonthlyUSDCents: stored.MonthlyUSDCents, Features: stored.Features, Allowances: stored.Allowances})
		if reflect.DeepEqual(existing, configured) {
			if err := w.SetBillingPlanRuntime(ctx, ref, revision.ID); err != nil {
				return err
			}
			continue
		}
		stored.Name, stored.MonthlyUSDCents = configured.Name, configured.MonthlyUSDCents
		stored.Features, stored.Allowances = configured.Features, configured.Allowances
		stored.RuntimeID = revision.ID
		if err := w.ReplaceBillingPlan(ctx, stored); err != nil {
			return err
		}
		audit := billingstore.PlanAudit{OrganizationID: d.hostedOrganization, CommandID: fmt.Sprintf("configuration_%s_%s_%d", revision.ID, ref.ID, ref.Version), RequestHash: revision.Reference.SHA256, ActorID: "configuration", Action: "update_plan", RecordedAt: d.now().UTC(), Reference: revision.Reference}
		if _, err := w.CreateBillingPlanAudit(ctx, audit); err != nil {
			return err
		}
	}
	return nil
}
