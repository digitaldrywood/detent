package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledSourceMaintenance struct {
	owns func(hubmaintenance.Tenant) bool
	step func(context.Context, hubmaintenance.Tenant, int) (int, error)
}

func (s *Service) newScheduledSourceMaintenance() *scheduledSourceMaintenance {
	owns := func(tenant hubmaintenance.Tenant) bool {
		return tenant.Organization == s.appOrganizationID() && (!s.hostedShared() || tenant.Generation == s.config.Hosted.SharedEntry.Generation)
	}
	feature := operatorcommands.NewScheduledSources(operatorcommands.ScheduledSourceDeps{
		Owns: owns,
		Store: func() operatorcommands.ScheduledSourceBackfiller {
			if owner, ok := s.config.TenantRepository.(operatorcommands.ScheduledSourceBackfiller); ok {
				return owner
			}
			return nil
		},
		Details: s.runDetailStore,
	})
	step := feature.Step
	if s.runtimeHost != nil {
		step = func(ctx context.Context, t hubmaintenance.Tenant, limit int) (int, error) {
			if err := hubmaintenance.RequireTenant(ctx, t); err != nil {
				return 0, err
			}
			if !owns(t) {
				return 0, corestore.ErrTenantMismatch
			}
			org := tracker.OrganizationID(t.Organization)
			repo := s.database.postgres
			cursor, err := repo.MaintenanceCursor(ctx, org, t.Generation, "scheduled-sources")
			if err != nil {
				return 0, err
			}
			after := postgres.ScheduledSourceCursor{ProjectID: tracker.ProjectID(cursor.Project), RecordID: cursor.Key}
			used, next, err := repo.BackfillScheduledSourcesAfter(ctx, org, eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore())), min(limit, corestore.MaxPageSize), after)
			if next != after {
				cursor.Project, cursor.Key = string(next.ProjectID), next.RecordID
				err = errors.Join(err, repo.SetMaintenanceCursor(ctx, org, t.Generation, "scheduled-sources", cursor))
			}
			return used, err
		}
	}
	return &scheduledSourceMaintenance{owns: owns, step: step}
}
