package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type publishedRuntimeCatalogWriter interface {
	RuntimePrepared(context.Context) (string, error)
	ApplyPublishedRuntimeCatalog(context.Context, billingstore.RuntimeRevision, []billingstore.Plan, []billingstore.Price, []billingstore.PlanAudit) error
}

func (d *database) applyRuntimeRecords(ctx context.Context, plans HostedPlansConfig, prices []HostedBillingPrice, actor, reason, stage string, validate bool) error {
	return d.applyRuntimeRecordsAtRevision(ctx, plans, prices, actor, reason, stage, validate, nil)
}

func (d *database) applyRuntimeRecordsAtRevision(ctx context.Context, plans HostedPlansConfig, prices []HostedBillingPrice, actor, reason, stage string, validate bool, expected *string) error {
	if d.billingAuditStore == nil {
		return errors.New("hosted billing audit storage is unavailable")
	}
	commands := []hostedPlanCommand{}
	for _, plan := range plans.Plans {
		plan = normalizeHostedPlan(plan)
		commands = append(commands, hostedPlanCommand{ID: "seed_" + uuid.NewString(), Action: "create_plan", Plan: plan.PlanReference, Definition: &plan, Reason: reason})
	}
	action := "reload"
	if stage != "" {
		action = "prepare_reload"
	}
	commands = append(commands, hostedPlanCommand{ID: "reload_" + uuid.NewString(), Action: action, Reason: reason})
	audits := make([]billingstore.PlanAudit, len(commands))
	for i, command := range commands {
		raw, err := json.Marshal(command)
		if err != nil {
			return err
		}
		ref, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		audits[i] = billingstore.PlanAudit{OrganizationID: d.hostedOrganization, CommandID: command.ID, RequestHash: hex.EncodeToString(digest[:]), ActorID: actor, Action: command.Action, RecordedAt: d.now().UTC(), Reference: billingstore.AuditReferenceFromArchive(ref)}
	}
	revision, err := d.archiveRuntime(ctx, plans, prices, stage, stage == "")
	if err != nil {
		return err
	}
	rollback := errors.New("runtime validation rollback")
	write := d.writeBillingTenant
	if expected != nil {
		write = func(ctx context.Context, apply func(tenantstore.Writer) error) error {
			return d.billingRepository.WriteTenant(ctx, d.hostedOrganization, corestore.Background, apply)
		}
	}
	err = write(ctx, func(w tenantstore.Writer) error {
		if expected != nil {
			writer, ok := w.(publishedRuntimeCatalogWriter)
			if !ok {
				return errors.New("published runtime catalog requires native tenant storage")
			}
			prepared, err := writer.RuntimePrepared(ctx)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if prepared == stage {
				published, err := w.RuntimePublished(ctx, stage)
				if err != nil {
					return err
				}
				if !published {
					return corestore.ErrRevisionConflict
				}
				return d.validateConfiguredPlanReferences(ctx, w, plans)
			}
			if prepared != *expected {
				return corestore.ErrRevisionConflict
			}
		}
		if err := d.validateConfiguredPlanReferences(ctx, w, plans); err != nil {
			return err
		}
		if expected != nil {
			writer, ok := w.(publishedRuntimeCatalogWriter)
			if !ok {
				return errors.New("published runtime catalog requires native tenant storage")
			}
			catalog := make([]billingstore.Plan, 0, len(plans.Plans))
			for _, plan := range plans.Plans {
				plan = normalizeHostedPlan(plan)
				catalog = append(catalog, billingstore.Plan{OrganizationID: d.hostedOrganization, PlanReference: billingstore.PlanReference{ID: plan.ID, Version: plan.Version}, Name: plan.Name, MonthlyUSDCents: plan.MonthlyUSDCents, Features: plan.Features, Allowances: plan.Allowances})
			}
			mappings := make([]billingstore.Price, 0, len(prices))
			for _, price := range prices {
				mappings = append(mappings, billingstore.Price{OrganizationID: d.hostedOrganization, PriceID: price.PriceID, Plan: billingstore.PlanReference{ID: price.Plan.ID, Version: price.Plan.Version}})
			}
			return writer.ApplyPublishedRuntimeCatalog(ctx, revision, catalog, mappings, audits)
		}
		for i, command := range commands[:len(commands)-1] {
			plan := *command.Definition
			ref := billingstore.PlanReference{ID: plan.ID, Version: plan.Version}
			stored, err := w.BillingPlan(ctx, ref)
			if errors.Is(err, corestore.ErrNotFound) {
				if err := w.CreateBillingPlan(ctx, billingstore.Plan{OrganizationID: d.hostedOrganization, PlanReference: ref, Name: plan.Name, MonthlyUSDCents: plan.MonthlyUSDCents, RuntimeID: revision.ID, Allowances: plan.Allowances, Features: plan.Features}); err != nil {
					return err
				}
				if _, err := w.CreateBillingPlanAudit(ctx, audits[i]); err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else {
				visible, err := w.RuntimePublished(ctx, stored.RuntimeID)
				if err != nil {
					return err
				}
				if !visible {
					if err := w.SetBillingPlanRuntime(ctx, ref, revision.ID); err != nil {
						return err
					}
				}
			}
		}
		for _, price := range prices {
			ref := billingstore.PlanReference{ID: price.Plan.ID, Version: price.Plan.Version}
			existing, err := w.BillingPrice(ctx, price.PriceID)
			if err == nil && existing.Plan != ref {
				return errors.New("hosted Stripe price mappings are immutable; configure a new price")
			}
			if errors.Is(err, corestore.ErrNotFound) {
				if err := w.CreateBillingPrice(ctx, billingstore.Price{OrganizationID: d.hostedOrganization, PriceID: price.PriceID, Plan: ref}); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
		if validate {
			return rollback
		}
		if _, err := w.CreateBillingPlanAudit(ctx, audits[len(audits)-1]); err != nil {
			return err
		}
		if err := w.StageRuntime(ctx, revision); err != nil {
			return err
		}
		published, err := w.RuntimePublished(ctx, revision.ID)
		if err == nil && !published && expected != nil {
			return corestore.ErrRevisionConflict
		}
		if err != nil || !published {
			return err
		}
		return d.replacePublishedPlans(ctx, w, revision, plans)
	})
	if errors.Is(err, rollback) {
		return nil
	}
	return err
}

func (d *database) validateConfiguredPlanReferences(ctx context.Context, r billingstore.Reader, plans HostedPlansConfig) error {
	refs := make([]billingstore.PlanReference, 0, len(plans.Plans))
	for _, plan := range plans.Plans {
		refs = append(refs, billingstore.PlanReference{ID: plan.ID, Version: plan.Version})
	}
	ref, err := r.MissingConfiguredBillingPlan(ctx, refs, d.now().UTC())
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return nativePublicInvalid(fmt.Sprintf("Hosted plan configuration omits referenced plan %s version %d; restore the definition before startup or reload", ref.ID, ref.Version))
}
