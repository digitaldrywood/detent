package hubserver

import (
	"context"
	"maps"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHealthDependencyRecords(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (healthProject, error) {
	p := healthProject{ID: string(project)}
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.ObservationReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		dependencies, err := owner.HealthDependencies(ctx, project, healthReadLimit+1)
		if err != nil {
			return err
		}
		if len(dependencies) > healthReadLimit {
			return errHealthReadLimit
		}
		gated, held, listed := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, dependency := range dependencies {
			gated[dependency.Dependent] = true
			if dependency.Dispatchable {
				continue
			}
			held[dependency.Blocker] = true
			if dependency.SameProject {
				listed[dependency.Blocker] = true
			}
			if p.HoldSince.IsZero() || dependency.Since.Before(p.HoldSince) {
				p.HoldSince = dependency.Since
			}
		}
		p.DependencyGated, p.HeldCount, p.HeldBlockers = len(gated), len(held), slices.Sorted(maps.Keys(listed))
		return nil
	})
	return p, err
}
