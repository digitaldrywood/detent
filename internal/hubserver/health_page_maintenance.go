package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) HealthPageMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "health-pages", Bytes: hubmaintenance.ByteLimit, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		_, err := s.MaintainHealthPage(ctx, tracker.OrganizationID(tenant.Organization))
		return err
	}}
}

func (s *Service) MaintainHealthPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), hubmaintenance.ByteLimit); err != nil {
		return false, err
	}
	var candidate healthstore.Finding
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.PageMaintainer)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		candidate, err = owner.NextHealthPage(ctx)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.PageMaintainer)
		if !ok {
			return workItemStorageUnavailable()
		}
		return owner.AdvanceHealthPage(ctx, candidate)
	})
	if err != nil {
		return false, err
	}
	var evidence healthstore.Evidence
	if candidate.Evidence.Key != "" {
		raw, err := hubmaintenance.LimitDetails(s.runDetailStore()).ReadDetail(ctx, string(organization), candidate.Evidence)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return false, err
		}
	}
	published := false
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.PageMaintainer)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		published, err = owner.PublishHealthPage(ctx, candidate, evidence)
		return err
	})
	if published && s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project health page", slog.String("organization_id", string(organization)), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.config.now().Sub(candidate.SourceAt)))
	}
	return published, err
}
