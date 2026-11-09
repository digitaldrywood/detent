package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorWorkRecord struct {
	plan         workItemRecordPlan
	fields       workItemRecordFields
	version      eventstore.CollaborationVersion
	event        eventstore.Event
	search       searchstore.Document
	attachments  []string
	commentBytes int64
}

func (s *Service) prepareCoordinatorWorkRecord(ctx context.Context, scope nativeScope, plan workItemRecordPlan, data tracker.CollaborationData) (coordinatorWorkRecord, error) {
	prepared := coordinatorWorkRecord{plan: plan}
	if plan.create {
		attachments, err := s.prepareWorkItemAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return prepared, err
		}
		prepared.attachments = attachments
	}
	fields, err := s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return prepared, err
	}
	prepared.fields = fields
	id, revision, item, at := string(plan.issue.WorkItemID), int64(plan.issue.Revision), plan.issue.WorkItemID, plan.issue.UpdatedAt
	text := searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels}
	document := searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, Kind: "issue", Scope: "issue"}
	if plan.comment.ID != "" {
		id, revision, item, at = plan.comment.ID, int64(plan.comment.Revision), plan.comment.WorkItemID, plan.comment.UpdatedAt
		document.Kind = "comment"
		text = searchstore.Text{Body: plan.comment.Body}
		data.CommentID = plan.comment.ID
	}
	raw, err := json.Marshal(plan.value())
	if err != nil {
		return prepared, err
	}
	if plan.comment.ID != "" && len(prepared.attachments) > 0 {
		data, err := json.Marshal(tracker.CollaborationData{Revision: plan.comment.Revision, CommentID: plan.comment.ID})
		if err != nil {
			return prepared, err
		}
		actor, err := json.Marshal(scope.actor())
		if err != nil {
			return prepared, err
		}
		prepared.commentBytes = int64(len(plan.comment.Body) + len(raw) + len(data) + len(actor))
	}
	content := eventstore.NewContent(s.runDetailStore())
	prepared.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, RecordID: id, Revision: revision}, raw)
	if err != nil {
		return prepared, err
	}
	document.ID, document.SourceID, document.WorkItemID, document.Revision, document.UpdatedAt = document.Kind+":"+id, id, item, revision, at
	prepared.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, document, text)
	if err != nil {
		return prepared, err
	}
	data.Revision, data.Fields = tracker.Revision(revision), plan.fields
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: item, Type: plan.eventType, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: at, Data: data})
	if err != nil {
		return prepared, err
	}
	prepared.event, err = content.PrepareEvent(ctx, scope.organization, raw)
	return prepared, err
}

func (p coordinatorWorkRecord) write(ctx context.Context, writer tenantstore.Writer, service *Service) error {
	if p.commentBytes > 0 {
		limit, err := service.attachmentQuotaLimit(ctx, writer)
		if err != nil {
			return err
		}
		usage, err := writer.AdjustCollaborationBytes(ctx, p.commentBytes, limit)
		if errors.Is(err, corestore.ErrNotFound) {
			return attachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
	}
	if p.plan.relationship != nil {
		if err := writer.SetRelationship(ctx, *p.plan.relationship, p.plan.relationshipOperate, p.plan.issue.UpdatedAt); err != nil {
			return err
		}
	}
	if err := writeWorkItemRecord(ctx, writer, p.plan, p.fields); err != nil {
		return err
	}
	previous := p.plan.current.Revision
	if p.plan.create {
		previous = 0
	}
	if err := writer.PutSearch(ctx, p.search, previous); err != nil {
		return err
	}
	if _, err := writer.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if _, err := writer.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	item := p.plan.issue.WorkItemID
	if p.plan.comment.ID != "" {
		item = p.plan.comment.WorkItemID
	}
	for _, id := range p.attachments {
		if err := writer.BindAttachment(ctx, attachmentstore.Reference{ProjectID: p.plan.issue.ProjectID, AttachmentID: id, WorkItemID: item, CommentID: p.plan.comment.ID}); err != nil {
			return err
		}
	}
	return nil
}
