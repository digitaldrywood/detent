package reports

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

func ReadFacets(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID, projects []tracker.ProjectID, w operatortool.AnalyticsWindow) (ReportsFacets, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	out := ReportsFacets{Labels: []string{}, FiledBy: []ReportsFacet{}, Runners: []ReportsFacet{}, Models: []string{}, Stages: []string{}}
	if repository == nil {
		return out, errors.New("PostgreSQL report reader is unavailable")
	}
	ownership, _ := ReportsScopeFrom(ctx)
	var facets reportstore.Facets
	err := repository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		store, ok := reader.(reportstore.Reader)
		if !ok {
			return errors.New("PostgreSQL report reader is unavailable")
		}
		var err error
		facets, err = store.ReadFacets(ctx, projects, w.From, w.To, ownership.ReporterPrincipal, ReportsFacetLimit)
		return err
	})
	if err != nil {
		return out, fmt.Errorf("read PostgreSQL report facets: %w", err)
	}
	return ReportsFacets{Labels: facets.Labels, FiledBy: ConvertFacets(facets.FiledBy), Runners: ConvertFacets(facets.Runners), Models: facets.Models, Stages: facets.Stages}, nil
}

func ConvertFacets(facets []reportstore.Facet) []ReportsFacet {
	out := make([]ReportsFacet, len(facets))
	for i, facet := range facets {
		out[i] = ReportsFacet{ID: facet.ID, Name: facet.Name}
	}
	return out
}
