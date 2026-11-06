package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledAttributionRecord struct {
	plan    workItemRecordPlan
	fields  workItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	search  searchstore.Document
}

func (s *Service) prepareScheduledAttributionRecord(ctx context.Context, scope nativeScope, plan workItemRecordPlan, data tracker.CollaborationData) (scheduledAttributionRecord, error) {
	p := scheduledAttributionRecord{plan: plan}
	var err error
	p.fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return p, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	id, revision, at := string(plan.issue.WorkItemID), int64(plan.issue.Revision), plan.issue.UpdatedAt
	kind := "issue"
	value := any(plan.issue)
	text := searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels}
	if plan.comment.ID != "" {
		id, revision, at, kind = plan.comment.ID, int64(plan.comment.Revision), plan.comment.UpdatedAt, "comment"
		value = plan.comment
		text = searchstore.Text{Body: plan.comment.Body}
		data.CommentID = plan.comment.ID
	}
	data.Revision = tracker.Revision(revision)
	raw, err := json.Marshal(value)
	if err != nil {
		return p, err
	}
	p.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.current.NativeID, RecordID: id, Revision: revision}, raw)
	if err != nil {
		return p, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: plan.current.NativeID, Type: plan.eventType, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: at, Data: data})
	if err != nil {
		return p, err
	}
	p.event, err = content.PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return p, err
	}
	p.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.current.NativeID, ID: kind + ":" + id, SourceID: id, Kind: kind, Scope: "issue", Revision: revision, UpdatedAt: at}, text)
	return p, err
}
