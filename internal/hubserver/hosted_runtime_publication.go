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
	d.runtimeReadMu.Lock()
	defer d.runtimeReadMu.Unlock()
	var revision billingstore.RuntimeRevision
	err := d.readBillingTenant(ctx, func(r tenantstore.Reader) error { var err error; revision, err = r.CurrentRuntime(ctx); return err })
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current := d.hostedRuntime.Load(); current != nil && current.Revision == revision.ID {
		return nil
	}
	raw, err := d.billingAuditStore.ReadDetail(ctx, string(d.hostedOrganization), revision.Reference.ArchiveReference())
	if err != nil {
		return err
	}
	var update HostedRuntimeUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return err
	}
	if update.Plans == nil {
		return workItemStorageUnavailable()
	}
	state := &hostedRuntimeConfig{Revision: revision.ID, Plans: *update.Plans}
	if d.runtimeBilling != nil {
		next := *d.runtimeBilling
		next.Prices = update.Prices
		state.Billing = &next
	}
	d.hostedRuntime.Store(state)
	return nil
}

func (s *Service) refreshHostedRuntime(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		if err := s.database.refreshRuntime(ctx); err != nil {
			return s.nativeAPIError(c, err)
		}
		return next(c)
	}
}
