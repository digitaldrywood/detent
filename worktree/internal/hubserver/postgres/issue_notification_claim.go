package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ accountstore.NotificationDelivery = (*Repository)(nil)

var errNotificationChanged = errors.New("notification changed")

func (r *Repository) NotificationSeek(ctx context.Context, org tracker.OrganizationID) (accountstore.NotificationCursor, error) {
	var seek accountstore.NotificationCursor
	err := r.transaction(ctx, org, corestore.Background, true, func(s *session) error {
		row, err := s.queries.NotificationSeek(ctx, org)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		seek = accountstore.NotificationCursor{WorkItemID: tracker.NativeWorkItemID(row.NoticeSeekWorkItemID), UserID: row.NoticeSeekUserID, Revision: row.NoticeSeekRevision}
		if row.NoticeSeekDueAt != nil {
			seek.DueAt = *row.NoticeSeekDueAt
		}
		return nil
	})
	return seek, err
}

func (r *Repository) AdvanceNotificationSeek(ctx context.Context, org tracker.OrganizationID, seek accountstore.NotificationCursor) (bool, error) {
	advanced := false
	err := r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		var due *time.Time
		if !seek.DueAt.IsZero() {
			due = &seek.DueAt
		}
		count, err := s.queries.AdvanceNotificationSeek(ctx, pgquery.AdvanceNotificationSeekParams{OrganizationID: org, DueAt: due, WorkItemID: string(seek.WorkItemID), UserID: seek.UserID, ExpectedRevision: seek.Revision})
		advanced = count == 1
		return mapError(ctx, err)
	})
	return advanced, err
}

func (r *Repository) DueNotification(ctx context.Context, org tracker.OrganizationID, now time.Time, cursor accountstore.NotificationCursor, passed []accountstore.NotificationCandidate) (accountstore.NotificationPage, error) {
	var page accountstore.NotificationPage
	err := r.transaction(ctx, org, corestore.Background, true, func(s *session) error {
		rows, err := s.queries.DueNotifications(ctx, pgquery.DueNotificationsParams{OrganizationID: org, DueAt: now, AfterDue: cursor.DueAt, AfterItem: string(cursor.WorkItemID), AfterUser: cursor.UserID})
		if err != nil {
			return mapError(ctx, err)
		}
		for _, row := range rows {
			item := tracker.NativeWorkItemID(row.WorkItemID)
			if slices.ContainsFunc(passed, func(candidate accountstore.NotificationCandidate) bool {
				return candidate.WorkItemID == item && candidate.UserID == row.UserID
			}) {
				continue
			}
			page.Candidate = accountstore.NotificationCandidate{WorkItemID: item, UserID: row.UserID, DueAt: row.DueAt}
			page.Next, page.Found = accountstore.NotificationCursor{DueAt: row.DueAt, WorkItemID: item, UserID: row.UserID}, true
			return nil
		}
		if len(rows) == 128 {
			last := rows[len(rows)-1]
			page.Next, page.More = accountstore.NotificationCursor{DueAt: last.DueAt, WorkItemID: tracker.NativeWorkItemID(last.WorkItemID), UserID: last.UserID}, true
		}
		return nil
	})
	return page, err
}

func (r *Repository) ClaimNotification(ctx context.Context, org tracker.OrganizationID, now time.Time, candidate accountstore.NotificationCandidate, pending bool) (accountstore.IssueNotification, accountstore.NotificationCandidate, bool, error) {
	var result accountstore.IssueNotification
	var unresolved accountstore.NotificationCandidate
	err := r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		subscription, err := s.queries.LockDueNotification(ctx, pgquery.LockDueNotificationParams{OrganizationID: org, WorkItemID: string(candidate.WorkItemID), UserID: candidate.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotificationChanged
		}
		if err != nil {
			return mapError(ctx, err)
		}
		due, err := s.queries.NotificationStillDue(ctx, pgquery.NotificationStillDueParams{OrganizationID: org, DueAt: candidate.DueAt, WorkItemID: subscription.WorkItemID, UserID: subscription.UserID})
		if err != nil {
			return mapError(ctx, err)
		}
		if !due {
			return errNotificationChanged
		}
		notices, err := s.queries.TakeIssueNotices(ctx, pgquery.TakeIssueNoticesParams{OrganizationID: org, WorkItemID: subscription.WorkItemID, UserID: subscription.UserID})
		if err != nil {
			return mapError(ctx, err)
		}
		if len(notices) == 0 {
			return errNotificationChanged
		}
		recipient, err := s.queries.NotificationRecipient(ctx, pgquery.NotificationRecipientParams{OrganizationID: org, WorkItemID: subscription.WorkItemID, UserID: subscription.UserID, ExpiresAt: &now})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		verify := slices.ContainsFunc(notices, func(notice pgquery.IssueNotificationNotice) bool { return notice.Verify })
		if verify && (!candidate.Verify || recipient.Revision != candidate.Revision) {
			unresolved = accountstore.NotificationCandidate{ProjectID: recipient.ProjectID, WorkItemID: candidate.WorkItemID, UserID: candidate.UserID, DueAt: candidate.DueAt, Revision: recipient.Revision, Verify: true}
			return errNotificationChanged
		}
		result = accountstore.IssueNotification{Email: recipient.Email, Title: recipient.Title, Token: recipient.Token, OrganizationSlug: recipient.OrganizationSlug, ProjectSlug: recipient.ProjectSlug, Number: int(recipient.Number)}
		slices.SortFunc(notices, func(a, b pgquery.IssueNotificationNotice) int {
			if n := a.DueAt.Compare(b.DueAt); n != 0 {
				return n
			}
			if n := strings.Compare(a.SourceItem, b.SourceItem); n != 0 {
				return n
			}
			return strings.Compare(a.EventID, b.EventID)
		})
		for _, notice := range notices {
			result.Notices = append(result.Notices, accountstore.IssueNotice{StateFrom: notice.StateFrom, StateTo: notice.StateTo, Author: notice.Author, CommentID: notice.CommentID, Related: notice.Related, Verify: notice.Verify && pending})
		}
		return nil
	})
	if errors.Is(err, errNotificationChanged) {
		return accountstore.IssueNotification{}, unresolved, false, nil
	}
	return result, unresolved, err == nil, err
}

func (r *Repository) UnsubscribeNotification(ctx context.Context, org tracker.OrganizationID, token string) error {
	return r.transaction(ctx, org, corestore.Serving, false, func(s *session) error {
		count, err := s.queries.UnsubscribeIssueNotification(ctx, pgquery.UnsubscribeIssueNotificationParams{OrganizationID: org, Token: token})
		if err != nil {
			return mapError(ctx, err)
		}
		if count == 0 {
			return corestore.ErrNotFound
		}
		return nil
	})
}
