package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (a hubChangeApplication) readChangeListRecords(ctx context.Context, args operatortool.ChangeArguments) (result operatortool.ChangeResult, resultErr error) {
	scope := a.scope
	scope.project = tracker.ProjectID(args.ProjectID)
	result = a.result(scope, args)
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin || scope.credential.Runner.RunnerID != "" {
		return result, operatortool.ErrAccessDenied
	}
	s := a.service
	if err := s.authorizeProjectRecord(ctx, scope, false); err != nil {
		return result, err
	}
	if _, _, err := s.readWorkItemRecord(ctx, scope, args.ItemID); err != nil {
		return result, err
	}
	ctx = s.archiveContext(ctx, scope, args.ArchiveURL)
	defer func() { result.Archive = nativeArchiveDescriptor(ctx) }()
	var records []eventstore.Change
	limit := changeLimit(args)
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		after := ""
		for remaining := args.Offset; remaining > 0; {
			rows, err := r.Changes(ctx, scope.project, tracker.NativeWorkItemID(args.ItemID), after, min(remaining, corestore.MaxPageSize))
			if err != nil || len(rows) == 0 {
				return err
			}
			remaining -= len(rows)
			after = rows[len(rows)-1].ID
		}
		var err error
		records, err = r.Changes(ctx, scope.project, tracker.NativeWorkItemID(args.ItemID), after, limit)
		if err != nil || len(records) < limit {
			return err
		}
		more, err := r.Changes(ctx, scope.project, tracker.NativeWorkItemID(args.ItemID), records[len(records)-1].ID, 1)
		if len(more) != 0 {
			next := args.Offset + limit
			result.NextOffset = &next
		}
		return err
	})
	if err != nil {
		return result, projectRecordError(err)
	}
	result.Changes = []tracker.ChangeRequest{}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, record := range records {
		change, err := content.Change(ctx, record)
		if err != nil {
			return result, err
		}
		change, visible, err := s.filterChangeRecord(ctx, scope, change)
		if err != nil {
			return result, err
		}
		if visible {
			result.Changes = append(result.Changes, change)
		}
	}
	return result, nil
}
