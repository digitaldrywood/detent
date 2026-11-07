package hubserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) ProjectPageMaintenanceFeature() hubmaintenance.Feature {
	type cursor struct {
		generation int64
		after      tracker.ProjectID
	}
	var mu sync.Mutex
	cursors := make(map[string]cursor)
	return hubmaintenance.Feature{Name: "project-pages", Bytes: hubmaintenance.ByteLimit, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		mu.Lock()
		current := cursors[tenant.Organization]
		mu.Unlock()
		if current.generation != tenant.Generation {
			current = cursor{generation: tenant.Generation}
		}
		next, _, err := s.MaintainProjectPage(ctx, tracker.OrganizationID(tenant.Organization), current.after)
		current.after = next
		mu.Lock()
		cursors[tenant.Organization] = current
		mu.Unlock()
		return err
	}}
}

func (s *Service) MaintainProjectPage(ctx context.Context, org tracker.OrganizationID, after tracker.ProjectID) (tracker.ProjectID, bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(org), hubmaintenance.ByteLimit); err != nil {
		return after, false, err
	}
	var candidate projectstore.PageCandidate
	err := s.config.TenantRepository.ReadTenant(ctx, org, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(projectstore.PageMaintainer)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		candidate, err = owner.NextProjectPage(ctx, after)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return after, false, err
	}
	next := candidate.ProjectID
	if candidate.Ready {
		return next, false, nil
	}
	ctx = context.WithValue(ctx, detailBatchContextKey{}, nil)
	ctx = context.WithValue(ctx, changeContentKey{}, nil)
	ctx = context.WithValue(ctx, runDetailContextKey{}, nil)
	page := projectstore.Page{}
	page.Body, err = s.projectRecordWorkflow(ctx, candidate.Record)
	if err != nil {
		return next, false, err
	}
	selection, err := s.projectRecordModel(ctx, candidate.Record, candidate.Models)
	if err != nil {
		return next, false, err
	}
	page.Model = selection.Effective
	published := false
	err = s.config.TenantRepository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.PageMaintainer)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		published, err = owner.PublishProjectPage(ctx, candidate, page)
		return err
	})
	if published && s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project settings page", slog.String("organization_id", string(org)), slog.Int64("generation", candidate.Record.Settings.Revision), slog.Duration("projection_lag", s.config.now().Sub(candidate.SourceAt)))
	}
	return next, published, err
}
