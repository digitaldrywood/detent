package changelanding

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reviewedChecklistProjection struct {
	list    *tracker.ExecutionChecklist
	item    corestore.WorkItem
	changes []tracker.ExecutionCheckUpdate
}

func readReviewedChecklist(ctx context.Context, repository tenantstore.Repository, scope hubscope.NativeScope, attempt string) (*tracker.ExecutionChecklist, error) {
	if attempt == "" {
		return nil, nil
	}
	var list *tracker.ExecutionChecklist
	err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(leasestore.ChecklistReader)
		if !ok {
			return nil
		}
		var err error
		list, err = reader.ExecutionChecklist(ctx, scope.Project, attempt)
		return err
	})
	return list, err
}

func (p *reviewedChecklistProjection) apply(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, evidence string, now time.Time) error {
	if p == nil {
		return nil
	}
	item, err := w.WorkItem(ctx, scope.Project, p.item.NativeID)
	if err != nil {
		return err
	}
	if hubapi.ValueOrEmpty(item.BodyObjectKey) != hubapi.ValueOrEmpty(p.item.BodyObjectKey) {
		return corestore.ErrRevisionConflict
	}
	reader, ok := w.(leasestore.ChecklistReader)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	current, err := reader.ExecutionChecklist(ctx, scope.Project, p.list.AttemptID)
	if err != nil {
		return err
	}
	if current == nil || current.Revision != p.list.Revision || !slices.Equal(current.Checks, p.list.Checks) {
		return corestore.ErrRevisionConflict
	}
	if len(p.changes) == 0 {
		return nil
	}
	writer, ok := w.(leasestore.ChecklistChangesWriter)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return writer.RecordExecutionCheckChanges(ctx, scope.Project, p.list.AttemptID, p.list.Revision, evidence, now, p.changes)
}
