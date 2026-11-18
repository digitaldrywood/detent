package reports

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func timePointer(value time.Time) *time.Time {
	return &value
}

type completionCostFailureReader struct {
	tenantstore.Reader
	err error
}

func (r completionCostFailureReader) ReadCompletionCosts(context.Context, reportstore.CompletionCostQuery) ([]reportstore.CompletionCostFact, error) {
	return nil, r.err
}

func slicesEqualFilter(a, b ReportsFilter) bool {
	return slices.Equal(a.Labels, b.Labels) && slices.Equal(a.Priorities, b.Priorities) && slices.Equal(a.Types, b.Types) && slices.Equal(a.FiledBy, b.FiledBy) && slices.Equal(a.Runners, b.Runners) && slices.Equal(a.Models, b.Models) && slices.Equal(a.Stages, b.Stages)
}
