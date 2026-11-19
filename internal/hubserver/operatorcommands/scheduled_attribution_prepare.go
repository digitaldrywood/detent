package operatorcommands

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ScheduledAttributionRecord struct {
	Plan    hubrecords.WorkItemRecordPlan
	Fields  hubrecords.WorkItemRecordFields
	Version eventstore.CollaborationVersion
	Event   eventstore.Event
}

func (s ScheduledAttributionDeps) PrepareRecord(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, data tracker.CollaborationData) (ScheduledAttributionRecord, error) {
	p := ScheduledAttributionRecord{Plan: plan}
	var err error
	p.Fields, err = s.PrepareFields(ctx, scope.Organization, plan)
	if err != nil {
		return p, err
	}
	content := eventstore.NewContent(s.Details())
	id, revision, at := string(plan.Issue.WorkItemID), int64(plan.Issue.Revision), plan.Issue.UpdatedAt
	value := any(plan.Issue)
	if plan.Comment.ID != "" {
		id, revision, at = plan.Comment.ID, int64(plan.Comment.Revision), plan.Comment.UpdatedAt
		value = plan.Comment
		data.CommentID = plan.Comment.ID
	}
	data.Revision = tracker.Revision(revision)
	raw, err := json.Marshal(value)
	if err != nil {
		return p, err
	}
	p.Version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Current.NativeID, RecordID: id, Revision: revision}, raw)
	if err != nil {
		return p, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: plan.Current.NativeID, Type: plan.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: at, Data: data})
	if err != nil {
		return p, err
	}
	p.Event, err = content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return p, err
	}
	return p, err
}
