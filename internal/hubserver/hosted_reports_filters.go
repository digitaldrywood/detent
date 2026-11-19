package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readReportsFacets(ctx context.Context, projects []string, w operatortool.AnalyticsWindow) (reportsFacets, error) {
	return reports.ReadFacets(ctx, s.config.TenantRepository, tracker.OrganizationID(s.appOrganizationID()), projectIDs(projects), w)
}
