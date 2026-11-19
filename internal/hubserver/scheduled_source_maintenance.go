package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
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
			owner, _ := s.config.TenantRepository.(operatorcommands.ScheduledSourceBackfiller)
			return owner
		},
		Details: s.runDetailStore,
	})
	return &scheduledSourceMaintenance{owns: owns, step: feature.Step}
}
