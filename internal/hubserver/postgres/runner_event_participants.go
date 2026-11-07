package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

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
