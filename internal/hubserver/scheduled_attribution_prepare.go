package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledAttributionRecord struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
}

func (s *Service) prepareScheduledAttributionRecord(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, data tracker.CollaborationData) (scheduledAttributionRecord, error) {
	deps := operatorcommands.ScheduledAttributionDeps{PrepareFields: s.prepareWorkItemFields, Details: s.runDetailStore}
	record, err := deps.PrepareRecord(ctx, scope, plan, data)
	return scheduledAttributionRecord{plan: record.Plan, fields: record.Fields, version: record.Version, event: record.Event}, err
}
