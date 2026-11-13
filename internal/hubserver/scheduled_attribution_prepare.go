package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledAttributionRecord struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	search  searchstore.Document
}

func (s *Service) prepareScheduledAttributionRecord(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, data tracker.CollaborationData) (scheduledAttributionRecord, error) {
	p := scheduledAttributionRecord{plan: plan}
	var err error
	p.fields, err = s.prepareWorkItemFields(ctx, scope.Organization, plan)
	if err != nil {
		return p, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	id, revision, at := string(plan.Issue.WorkItemID), int64(plan.Issue.Revision), plan.Issue.UpdatedAt
	kind := "issue"
	value := any(plan.Issue)
	text := searchstore.Text{Title: plan.Issue.Title, Body: plan.Issue.Body, Labels: plan.Issue.Labels}
	if plan.Comment.ID != "" {
		id, revision, at, kind = plan.Comment.ID, int64(plan.Comment.Revision), plan.Comment.UpdatedAt, "comment"
		value = plan.Comment
		text = searchstore.Text{Body: plan.Comment.Body}
		data.CommentID = plan.Comment.ID
	}
	data.Revision = tracker.Revision(revision)
	raw, err := json.Marshal(value)
	if err != nil {
		return p, err
	}
	p.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Current.NativeID, RecordID: id, Revision: revision}, raw)
	if err != nil {
		return p, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: plan.Current.NativeID, Type: plan.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: at, Data: data})
	if err != nil {
		return p, err
	}
	p.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return p, err
	}
	p.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Current.NativeID, ID: kind + ":" + id, SourceID: id, Kind: kind, Scope: "issue", Revision: revision, UpdatedAt: at}, text)
	return p, err
}
