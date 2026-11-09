package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorTransitionAnswer struct {
	checkAttempt bool
	attempt      leasestore.Attempt
	plan         *workItemRecordPlan
	fields       workItemRecordFields
	version      eventstore.CollaborationVersion
	event        eventstore.Event
	search       searchstore.Document
}

func (s *Service) prepareCoordinatorTransitionAnswer(ctx context.Context, scope nativeScope, row corestore.WorkItem, issue tracker.NativeIssue, request tracker.Transition, conversationID string) (coordinatorTransitionAnswer, error) {
	var prepared coordinatorTransitionAnswer
	if conversationID == "" {
		return prepared, nil
	}
	var err error
	if strings.EqualFold(issue.State, "Blocked") && !strings.EqualFold(issue.State, request.State) {
		prepared.checkAttempt = true
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			prepared.attempt, err = r.LatestAttempt(ctx, row.ID)
			if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		})
		if err != nil {
			return prepared, err
		}
		if prepared.attempt.ID != "" {
			attempt, err := s.readNativeAttemptTenant(ctx, scope, string(issue.WorkItemID), prepared.attempt.ID)
			if err != nil {
				return prepared, err
			}
			if attempt.Disposition != nil && attempt.Disposition.Status == "blocked" && attempt.Disposition.HumanAction && strings.TrimSpace(request.ReasonDetail) == "" {
				return prepared, nativePublicInvalid("Provide the human's answer to the recorded human_action question in reason_detail before moving this item out of Blocked")
			}
		}
	}

	body := strings.TrimSpace(request.ReasonDetail)
	if body == "" {
		return prepared, nil
	}
	if len(body) > 64<<10 {
		return prepared, nativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
	}
	now := s.config.now().UTC()
	comment := tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, ID: newNativeID("cmt"), Revision: 1, Sequence: row.EventSequence + 1, Body: body, Actor: scope.actor(), Provenance: &tracker.Provenance{Provider: "chat", ExternalID: conversationID + ":" + request.IdempotencyKey, AuthorID: scope.actor().PrincipalID}, CreatedAt: now, UpdatedAt: now}
	plan := workItemRecordPlan{current: row, issue: issue, comment: comment, create: true, eventType: "comment.created"}
	prepared.plan = &plan
	prepared.fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return prepared, err
	}
	raw, err := json.Marshal(comment)
	if err != nil {
		return prepared, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	prepared.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, RecordID: comment.ID, Revision: 1}, raw)
	if err != nil {
		return prepared, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "comment.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: 1, CommentID: comment.ID}})
	if err != nil {
		return prepared, err
	}
	prepared.event, err = content.PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return prepared, err
	}
	prepared.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, ID: "comment:" + comment.ID, SourceID: comment.ID, Kind: "comment", Scope: "issue", Revision: 1, UpdatedAt: now}, searchstore.Text{Body: body})
	return prepared, err
}

func (p coordinatorTransitionAnswer) validate(ctx context.Context, reader tenantstore.Reader, issueID int64) error {
	if !p.checkAttempt {
		return nil
	}
	current, err := reader.LatestAttempt(ctx, issueID)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		current, err = leasestore.Attempt{}, nil
	}
	if err != nil {
		return err
	}
	if current.ID != p.attempt.ID || current.Sequence != p.attempt.Sequence {
		return corestore.ErrSerializationFailure
	}
	return nil
}

func (p coordinatorTransitionAnswer) write(ctx context.Context, writer tenantstore.Writer) error {
	if p.plan == nil {
		return nil
	}
	if err := writeWorkItemRecord(ctx, writer, *p.plan, p.fields); err != nil {
		return err
	}
	if _, err := writer.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if err := writer.PutSearch(ctx, p.search, 0); err != nil {
		return err
	}
	_, err := writer.AppendEvent(ctx, p.event)
	return err
}
