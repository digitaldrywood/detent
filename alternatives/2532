package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ accountstore.NotificationCollector = (*Repository)(nil)

func (r *Repository) NotificationEvents(ctx context.Context, org tracker.OrganizationID) ([]eventstore.Event, error) {
	result := []eventstore.Event{}
	err := r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		projects, err := s.queries.LockNotificationProject(ctx, org)
		if err != nil {
			return mapError(ctx, err)
		}
		if len(projects) == 0 {
			return nil
		}
		project := projects[0]
		rows, err := s.queries.NotificationEvents(ctx, pgquery.NotificationEventsParams{OrganizationID: org, ProjectID: project.ProjectID, ProjectSequence: project.NotificationSequence})
		if err != nil {
			return mapError(ctx, err)
		}
		after := project.NotificationSequence
		for _, row := range rows {
			if project.Native && row.CollaborationEvent.Sequence > row.ConsumedSequence {
				result = append(result, eventRecord(row.CollaborationEvent))
			} else if len(result) == 0 {
				after = row.CollaborationEvent.ProjectSequence
			}
		}
		if len(result) == 0 && len(rows) < 256 {
			after = project.Sequence
		}
		return mapError(ctx, s.queries.AdvanceNotificationProject(ctx, pgquery.AdvanceNotificationProjectParams{OrganizationID: org, ProjectID: project.ProjectID, NotificationSequence: after}))
	})
	return result, err
}

func (r *Repository) QueueNotification(ctx context.Context, org tracker.OrganizationID, event eventstore.Event, notice accountstore.IssueNotice, related tracker.NativeWorkItemID, notify bool, limit int) (bool, int, error) {
	if event.OrganizationID != org || event.Sequence < 1 {
		return false, 0, corestore.ErrTenantMismatch
	}
	if limit < 1 || limit > 256 {
		return false, 0, corestore.ErrInvalidPage
	}
	processed := 0
	complete := false
	err := r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		if err := s.queries.EnsureNotificationCursor(ctx, pgquery.EnsureNotificationCursorParams{OrganizationID: org, WorkItemID: string(event.WorkItemID)}); err != nil {
			return mapError(ctx, err)
		}
		cursor, err := s.queries.LockNotificationCursor(ctx, pgquery.LockNotificationCursorParams{OrganizationID: org, WorkItemID: string(event.WorkItemID)})
		if err != nil {
			return mapError(ctx, err)
		}
		if cursor.Sequence >= event.Sequence {
			complete = true
			return nil
		}
		events, err := s.Events(ctx, event.ProjectID, event.WorkItemID, cursor.Sequence, 1)
		if err != nil {
			return err
		}
		if len(events) != 1 || events[0].ID != event.ID {
			return corestore.ErrRevisionConflict
		}
		if notice.CommentID != "" {
			author, err := s.queries.NotificationAuthor(ctx, pgquery.NotificationAuthorParams{OrganizationID: org, PrincipalID: event.ActorPrincipalID})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return mapError(ctx, err)
			}
			if author != "" {
				notice.Author = author
			}
		}
		targets := []tracker.NativeWorkItemID{event.WorkItemID}
		if notice.Related && related != "" && related != event.WorkItemID {
			targets = append(targets, related)
		}
		budget := limit
		for _, item := range targets {
			if !notify {
				break
			}
			if cursor.RecipientItem != "" && cursor.RecipientItem != string(item) {
				continue
			}
			if budget == 0 {
				return mapError(ctx, s.queries.AdvanceNotificationCursor(ctx, pgquery.AdvanceNotificationCursorParams{OrganizationID: org, WorkItemID: string(event.WorkItemID), Sequence: cursor.Sequence, RecipientItem: string(item)}))
			}
			next, count, err := s.queueNotificationRecipients(ctx, event, item, notice, cursor.RecipientPrincipal, budget)
			if err != nil {
				return err
			}
			budget -= count
			processed += count
			if next != "" {
				return mapError(ctx, s.queries.AdvanceNotificationCursor(ctx, pgquery.AdvanceNotificationCursorParams{OrganizationID: org, WorkItemID: string(event.WorkItemID), Sequence: cursor.Sequence, RecipientItem: string(item), RecipientPrincipal: next}))
			}
			cursor.RecipientItem, cursor.RecipientPrincipal = "", ""
		}
		complete = true
		return mapError(ctx, s.queries.AdvanceNotificationCursor(ctx, pgquery.AdvanceNotificationCursorParams{OrganizationID: org, WorkItemID: string(event.WorkItemID), Sequence: event.Sequence}))
	})
	return complete, processed, err
}

func (s *session) queueNotificationRecipients(ctx context.Context, event eventstore.Event, item tracker.NativeWorkItemID, notice accountstore.IssueNotice, after string, budget int) (string, int, error) {
	if budget == 0 {
		return after, 0, nil
	}
	rows, err := s.queries.NotificationRecipients(ctx, pgquery.NotificationRecipientsParams{OrganizationID: s.organization, WorkItemID: item, RecordedAt: event.RecordedAt, EventItem: event.WorkItemID, Sequence: event.Sequence, AfterPrincipal: after, NowAt: time.Now().UTC()})
	if err != nil {
		return "", 0, mapError(ctx, err)
	}
	next := ""
	if len(rows) > budget {
		next = rows[budget-1].Principal
		rows = rows[:budget]
	}
	for _, row := range rows {
		if !row.Eligible || event.ActorKind == "human" && row.Principal == event.ActorPrincipalID {
			continue
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", 0, err
		}
		if err := s.queries.EnsureIssueSubscription(ctx, pgquery.EnsureIssueSubscriptionParams{OrganizationID: s.organization, WorkItemID: string(item), UserID: row.UserID, Token: "unsub_" + hex.EncodeToString(random[:])}); err != nil {
			return "", 0, mapError(ctx, err)
		}
		if err := s.queries.AppendIssueNotification(ctx, pgquery.AppendIssueNotificationParams{OrganizationID: s.organization, WorkItemID: string(item), UserID: row.UserID, EventID: event.ID, SourceItem: string(event.WorkItemID), DueAt: event.RecordedAt.Add(time.Minute), StateFrom: notice.StateFrom, StateTo: notice.StateTo, Author: notice.Author, CommentID: notice.CommentID, Related: notice.Related, Verify: notice.Verify && row.Filer && item == event.WorkItemID}); err != nil {
			return "", 0, mapError(ctx, err)
		}
	}
	return next, len(rows), nil
}
