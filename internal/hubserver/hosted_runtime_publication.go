package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) archiveRuntime(ctx context.Context, plans HostedPlansConfig, prices []HostedBillingPrice, id string, published bool) (billingstore.RuntimeRevision, error) {
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

func (d *database) refreshRuntime(ctx context.Context) error {
	if d.billingRepository == nil {
		return nil
	}
	current := d.hostedRuntime.Load()
	var revision billingstore.RuntimeRevision
	err := d.readBillingTenant(ctx, func(r tenantstore.Reader) error { var err error; revision, err = r.CurrentRuntime(ctx); return err })
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
			return nil, workItemStorageUnavailable()
		}
		if err := d.applyPublishedPlans(ctx, revision, *update.Plans); err != nil {
			return nil, err
		}
		state := &hostedRuntimeConfig{Revision: revision.ID, Plans: *update.Plans}
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

func (s *Service) refreshHostedRuntime(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
			return next(c)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		if err := s.database.refreshRuntime(ctx); err != nil {
			s.config.Logger.WarnContext(ctx, "hosted runtime refresh failed; retaining last snapshot", "error", err)
		}
		return next(c)
	}
}

func (d *database) applyPublishedPlans(ctx context.Context, revision billingstore.RuntimeRevision, plans HostedPlansConfig) error {
	return d.writeBillingTenant(ctx, func(w tenantstore.Writer) error {
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

func (d *database) replacePublishedPlans(ctx context.Context, w tenantstore.Writer, revision billingstore.RuntimeRevision, plans HostedPlansConfig) error {
	for _, configured := range plans.Plans {
		configured = normalizeHostedPlan(configured)
		ref := billingstore.PlanReference{ID: configured.ID, Version: configured.Version}
		stored, err := w.BillingPlan(ctx, ref)
		if err != nil {
			return err
		}
		existing := normalizeHostedPlan(HostedPlan{PlanReference: configured.PlanReference, Name: stored.Name, MonthlyUSDCents: stored.MonthlyUSDCents, Features: stored.Features, Allowances: stored.Allowances})
		if reflect.DeepEqual(existing, configured) {
			if stored.RuntimeID == "" {
				if err := w.SetBillingPlanRuntime(ctx, ref, revision.ID); err != nil {
					return err
				}
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
