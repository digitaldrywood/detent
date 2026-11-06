package hubserver

import (
	"context"
	"encoding/json"
	"errors"

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
