package hubserver

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type summaryCostRepository struct {
	tenantstore.Repository
	reader *summaryCostReader
}

func (r summaryCostRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return read(r.reader)
}

type summaryCostReader struct {
	tenantstore.Reader
	facts     []reportstore.OverviewFact
	usage     []reportstore.UsageFact
	principal string
}

func (r *summaryCostReader) ReadOverviewFacts(_ context.Context, q reportstore.OverviewQuery) ([]reportstore.OverviewFact, error) {
	r.principal = q.Principal
	return r.facts, nil
}
func (r *summaryCostReader) ReadFacets(context.Context, []tracker.ProjectID, time.Time, time.Time, string, int) (reportstore.Facets, error) {
	panic("unexpected facets read")
}
func (r *summaryCostReader) HasPriorData(context.Context, []tracker.ProjectID, time.Time, string) (bool, error) {
	panic("unexpected prior read")
}
func (r *summaryCostReader) ReadSummaryUsage(_ context.Context, project tracker.ProjectID, principal string, from, to time.Time, lifetime []tracker.NativeWorkItemID) ([]reportstore.UsageFact, error) {
	if principal != r.principal {
		panic("summary visibility changed")
	}
	return r.usage, nil
}

func TestSummaryTypedCostsPreserveCompletionAndUsageWindows(t *testing.T) {
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for _, test := range []struct {
		name, state, currency string
		wantShipped           int
		wantError             bool
	}{{"completed", "Done", "usd", 1, false}, {"reopened", "Rework", "usd", 0, false}, {"invalid currency", "Done", "eur", 0, true}} {
		t.Run(test.name, func(t *testing.T) {
			reader := &summaryCostReader{facts: []reportstore.OverviewFact{{ProjectID: "example", WorkItemID: "shipped", State: test.state, CompletedState: "Done", CompletedAt: &start}, {ProjectID: "example", WorkItemID: "boundary", State: "Done", CompletedState: "Done", CompletedAt: &end}}}
			for _, row := range []struct {
				id   string
				at   time.Time
				cost float64
			}{{"shipped", start.Add(-time.Hour), 7}, {"shipped", start, 3}, {"open", start.Add(time.Hour), 5}, {"boundary", end, 11}} {
				reader.usage = append(reader.usage, reportstore.UsageFact{ProjectID: "example", WorkItemID: tracker.NativeWorkItemID(row.id), AttemptID: "attempt_" + row.id, Period: row.at, Cost: row.cost, Currency: test.currency, Input: 10, Output: 2})
			}
			service := &Service{config: Config{TenantRepository: summaryCostRepository{reader: reader}}}
			got, err := service.readSummaryCostsTenant(t.Context(), nativeScope{organization: "org_example", project: "example"}, "principal_example", usageWindow{From: start, To: end})
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v", err)
			}
			if test.wantError {
				return
			}
			wantShippedSpend, wantNotShipped := float64(10), float64(5)
			if test.wantShipped == 0 {
				wantShippedSpend, wantNotShipped = 0, 8
			}
			if len(got.shipped) != test.wantShipped || got.spend != 8 || got.tokens != 24 || got.shippedSpend != wantShippedSpend || got.notShipped != wantNotShipped || reader.principal != "principal_example" {
				t.Fatalf("costs=%+v principal=%s", got, reader.principal)
			}
		})
	}
}
