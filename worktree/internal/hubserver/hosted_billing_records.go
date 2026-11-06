package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) readHostedPlanRecord(ctx context.Context, r billingstore.Reader, ref PlanReference) (HostedPlan, error) {
	return d.loadHostedPlan(ctx, ref, func(ctx context.Context, ref PlanReference) (HostedPlan, string, error) {
		row, err := r.BillingPlan(ctx, billingstore.PlanReference{ID: ref.ID, Version: ref.Version})
		if err == nil {
			var visible bool
			visible, err = r.RuntimePublished(ctx, row.RuntimeID)
			if err == nil && !visible {
				err = corestore.ErrNotFound
			}
		}
		if errors.Is(err, corestore.ErrNotFound) {
			err = sql.ErrNoRows
		}
		return HostedPlan{PlanReference: ref, Name: row.Name, MonthlyUSDCents: row.MonthlyUSDCents, Features: row.Features, Allowances: row.Allowances}, row.RuntimeID, err
	})
}

func (d *database) readBillingTenant(ctx context.Context, run func(tenantstore.Reader) error) error {
	if d.billingRepository == nil {
		return errors.New("billing repository is unavailable")
	}
	return d.billingRepository.ReadTenant(ctx, d.hostedOrganization, corestore.Serving, run)
}

func (d *database) writeBillingTenant(ctx context.Context, run func(tenantstore.Writer) error) error {
	if d.billingRepository == nil {
		return errors.New("billing repository is unavailable")
	}
	return d.billingRepository.WriteTenant(ctx, d.hostedOrganization, corestore.Serving, run)
}

func (d *database) archiveBillingReason(ctx context.Context, reason string) (billingstore.AuditReference, error) {
	if d.billingAuditStore == nil {
		return billingstore.AuditReference{}, errors.New("billing detail storage is unavailable")
	}
	raw, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return billingstore.AuditReference{}, err
	}
	archive, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
	if err != nil {
		return billingstore.AuditReference{}, err
	}
	return billingstore.AuditReferenceFromArchive(archive), nil
}

func (d *database) readBillingReason(ctx context.Context, ref billingstore.AuditReference) (string, error) {
	if ref.Key == "" {
		return "", nil
	}
	if d.billingAuditStore == nil {
		return "", errors.New("billing detail storage is unavailable")
	}
	raw, err := d.billingAuditStore.ReadDetail(ctx, string(d.hostedOrganization), ref.ArchiveReference())
	if err != nil {
		return "", err
	}
	var detail struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return "", err
	}
	return detail.Reason, nil
}

func (d *database) hostedEntitlementRecord(ctx context.Context, r billingstore.Reader, now time.Time) (HostedEntitlement, error) {
	result := HostedEntitlement{OrganizationID: string(d.hostedOrganization), Source: "base", Grants: []HostedGrant{}}
	if d.plans() == nil {
		return result, errors.New("hosted entitlements are disabled")
	}
	assignment, err := r.BillingAssignment(ctx)
	if err != nil {
		return result, err
	}
	result.Base = PlanReference{ID: assignment.Base.ID, Version: assignment.Base.Version}
	result.EffectiveBase, result.Revision = result.Base, assignment.Revision
	if assignment.Subscription != nil && assignment.ExpiresAt != nil && assignment.ExpiresAt.After(now) {
		result.EffectiveBase = PlanReference{ID: assignment.Subscription.ID, Version: assignment.Subscription.Version}
		result.Source = "subscription"
	}
	grants, err := r.BillingEffectiveGrants(ctx, now)
	if err != nil {
		return result, err
	}
	for _, grant := range grants {
		result.Grants = append(result.Grants, HostedGrant{ID: grant.ID, Plan: PlanReference{ID: grant.Plan.ID, Version: grant.Plan.Version}, Scope: grant.Scope, StartsAt: grant.StartsAt, ExpiresAt: grant.ExpiresAt, RevokedAt: grant.RevokedAt})
	}
	return d.resolveHostedEntitlement(ctx, now, result, func(ctx context.Context, ref PlanReference) (HostedPlan, error) {
		return d.readHostedPlanRecord(ctx, r, ref)
	})
}
