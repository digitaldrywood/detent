package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (f *runnerEventSession) RecordBillingCosts(ctx context.Context, records []billingstore.CostObservation) ([]bool, error) {
	if err := f.flushRunEvents(ctx); err != nil {
		return nil, err
	}
	return f.Writer.RecordBillingCosts(ctx, records)
}
func (f *runnerEventSession) RefreshReportUsage(ctx context.Context, records []billingstore.CostObservation) error {
	if err := f.flushRunEvents(ctx); err != nil {
		return err
	}
	writer, ok := f.Writer.(reportstore.UsageWriter)
	if !ok {
		return corestore.ErrInvalidPage
	}
	return writer.RefreshReportUsage(ctx, records)
}
func (f *runnerEventSession) WithRunEventParticipant(ctx context.Context, apply func(tenantstore.Writer) error) error {
	if err := f.flushRunEvents(ctx); err != nil {
		return err
	}
	if err := apply(f.Writer); err != nil {
		return err
	}
	item, err := f.Writer.WorkItem(ctx, f.command.Project, f.command.WorkItem)
	if err != nil {
		return err
	}
	f.item = runFact[pgquery.Issue]{value: pgquery.Issue(item)}
	return nil
}
