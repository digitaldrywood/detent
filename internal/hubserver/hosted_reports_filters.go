package hubserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readReportsFacets(ctx context.Context, projects []string, w operatortool.AnalyticsWindow) (reportsFacets, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	out := reportsFacets{Labels: []string{}, FiledBy: []reportsFacet{}, Runners: []reportsFacet{}, Models: []string{}, Stages: []string{}}
	if s.config.TenantRepository == nil {
		return out, errors.New("PostgreSQL report reader is unavailable")
	}
	ownership, _ := reportsScopeFrom(ctx)
	var facets reportstore.Facets
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.appOrganizationID()), corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return errors.New("PostgreSQL report reader is unavailable")
		}
		var err error
		facets, err = store.ReadFacets(ctx, projectIDs(projects), w.From, w.To, ownership.ReporterPrincipal, reportsFacetLimit)
		return err
	})
	if err != nil {
		return out, fmt.Errorf("read PostgreSQL report facets: %w", err)
	}
	return reportsFacets{Labels: facets.Labels, FiledBy: convertReportFacets(facets.FiledBy), Runners: convertReportFacets(facets.Runners), Models: facets.Models, Stages: facets.Stages}, nil
}
