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

func (r *Repository) ClaimNotification(ctx context.Context, org tracker.OrganizationID, now time.Time) (accountstore.IssueNotification, bool, error) {
	var result accountstore.IssueNotification
	found := false
	err := r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		subscription, err := s.queries.LockDueNotification(ctx, pgquery.LockDueNotificationParams{OrganizationID: org, DueAt: now})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
		}
		notices, err := s.queries.TakeIssueNotices(ctx, pgquery.TakeIssueNoticesParams{OrganizationID: org, WorkItemID: subscription.WorkItemID, UserID: subscription.UserID})
		if err != nil {
			return mapError(ctx, err)
		}
		found = true
		recipient, err := s.queries.NotificationRecipient(ctx, pgquery.NotificationRecipientParams{OrganizationID: org, WorkItemID: subscription.WorkItemID, UserID: subscription.UserID, ExpiresAt: &now})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return mapError(ctx, err)
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
			result.Notices = append(result.Notices, accountstore.IssueNotice{StateFrom: notice.StateFrom, StateTo: notice.StateTo, Author: notice.Author, CommentID: notice.CommentID, Related: notice.Related, Verify: notice.Verify && recipient.Verification == tracker.VerificationPending})
		}
		return nil
	})
	return result, found, err
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
