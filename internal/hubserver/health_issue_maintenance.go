package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const healthIssueMaintenanceBytes = 256 << 20

func (s *Service) HealthIssueMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "health-issues", Bytes: healthIssueMaintenanceBytes, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		if err := hubmaintenance.RequireAllowance(ctx, tenant.Organization, healthIssueMaintenanceBytes); err != nil {
			return err
		}
		if _, ok := s.runDetailStore().(*hubmaintenance.Details); !ok {
			return errors.New("maintenance requires byte-limited payload storage")
		}
		_, err := s.maintainHealthIssue(ctx, tracker.OrganizationID(tenant.Organization), s.config.now())
		return err
	}}
}
