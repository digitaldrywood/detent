package hubserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubserver/detailbatch"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) projectionFeature(name string, bytes int64, step func(context.Context, hubmaintenance.Tenant, hubmaintenance.Budget, *postgres.MaintenanceCursor) (hubmaintenance.Outcome, error)) hubmaintenance.Feature {
	var mu sync.Mutex
	type localCursor struct {
		generation int64
		cursor     postgres.MaintenanceCursor
	}
	local := make(map[string]localCursor)
	return hubmaintenance.Feature{Name: name, Bytes: bytes, Step: func(ctx context.Context, t hubmaintenance.Tenant, b hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		if err := hubmaintenance.RequireTenant(ctx, t); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		if s.runtimeHost != nil {
			org := tracker.OrganizationID(t.Organization)
			prior, err := s.database.postgres.MaintenanceCursor(ctx, org, t.Generation, name)
			if err != nil {
				return hubmaintenance.Outcome{}, err
			}
			cursor := prior
			outcome, err := step(ctx, t, b, &cursor)
			if cursor != prior {
				err = errors.Join(err, s.database.postgres.SetMaintenanceCursor(ctx, org, t.Generation, name, cursor))
			}
			return outcome, err
		}
		mu.Lock()
		current := local[t.Organization]
		mu.Unlock()
		if current.generation != t.Generation {
			current = localCursor{generation: t.Generation}
		}
		outcome, err := step(ctx, t, b, &current.cursor)
		mu.Lock()
		local[t.Organization] = current
		mu.Unlock()
		return outcome, err
	}}
}

func (s *Service) ProjectPageMaintenanceFeature() hubmaintenance.Feature {
	return s.projectionFeature("project-pages", hubmaintenance.ByteLimit, func(ctx context.Context, t hubmaintenance.Tenant, _ hubmaintenance.Budget, c *postgres.MaintenanceCursor) (hubmaintenance.Outcome, error) {
		next, worked, err := s.MaintainProjectPage(ctx, tracker.OrganizationID(t.Organization), tracker.ProjectID(c.Project))
		c.Project = string(next)
		return hubmaintenance.Outcome{Progress: worked, Items: 1}, err
	})
}

func (s *Service) MaintainProjectPage(ctx context.Context, org tracker.OrganizationID, after tracker.ProjectID) (tracker.ProjectID, bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(org), hubmaintenance.ByteLimit); err != nil {
		return after, false, err
	}
	var candidate projectstore.PageCandidate
	err := s.config.TenantRepository.ReadTenant(ctx, org, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(projectstore.PageMaintainer)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
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
	ctx = detailbatch.Clear(ctx)
	ctx = context.WithValue(ctx, hubcontent.ChangeContentKey{}, nil)
	ctx = context.WithValue(ctx, hubcontent.RunDetailContextKey{}, nil)
	page := projectstore.Page{}
	page.Body, err = s.projectWorkflowMarkdown(ctx, candidate.Record.Settings)
	if err != nil {
		return next, false, err
	}
	selection, err := s.projectRecordModel(ctx, candidate.Record.Settings, candidate.Models)
	if err != nil {
		return next, false, err
	}
	page.Model = selection.Effective
	page.ReporterTemplates = config.DefaultReporterContracts()
	if candidate.Policy != nil {
		approval, err := s.decodeTenantProjectPolicy(ctx, org, candidate.ProjectID, *candidate.Policy)
		if err != nil {
			return next, false, err
		}
		page.ReporterTemplates, err = approvedReporterTemplates(ctx, org, candidate.ProjectID, approval)
		if err != nil {
			return next, false, err
		}
	}
	published := false
	err = s.config.TenantRepository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.PageMaintainer)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		published, err = owner.PublishProjectPage(ctx, candidate, page)
		return err
	})
	if published && s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project settings page", slog.String("organization_id", string(org)), slog.Int64("generation", candidate.Record.Settings.Revision), slog.Duration("projection_lag", s.config.Clock().Sub(candidate.SourceAt)))
	}
	return next, published, err
}
