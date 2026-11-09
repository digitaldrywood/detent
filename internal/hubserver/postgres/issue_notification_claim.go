package postgres

import (
	"context"
	"crypto/rand"
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
		receipt, err := s.rejectedNotification(ctx, now)
		retry := err == nil
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		var notices []pgquery.IssueNotificationNotice
		if retry {
			notices, err = s.notificationReceiptNotices(ctx, receipt)
			if err != nil {
				return err
			}
			receipt.Attempts++
			_, err = s.db.Exec(ctx, `UPDATE issue_notification_receipts SET status='uncertain',attempts=$3,claimed_at=$4,finished_at=NULL WHERE organization_id=$1 AND id=$2`, org, receipt.ID, receipt.Attempts, now)
			if err != nil {
				return mapError(ctx, err)
			}
		} else {
			subscription, err := s.queries.LockDueNotification(ctx, pgquery.LockDueNotificationParams{OrganizationID: org, DueAt: now})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return mapError(ctx, err)
			}
			notices, err = s.queries.TakeIssueNotices(ctx, pgquery.TakeIssueNoticesParams{OrganizationID: org, WorkItemID: subscription.WorkItemID, UserID: subscription.UserID})
			if err != nil {
				return mapError(ctx, err)
			}
			if len(notices) == 0 {
				return corestore.ErrRevisionConflict
			}
			receipt = accountstore.NotificationReceipt{ID: "notification_" + rand.Text(), WorkItemID: tracker.NativeWorkItemID(subscription.WorkItemID), UserID: subscription.UserID, Status: "uncertain", Attempts: 1, NoticeCount: len(notices), ClaimedAt: now, NextAttemptAt: now}
			_, err = s.db.Exec(ctx, `INSERT INTO issue_notification_receipts(organization_id,id,work_item_id,user_id,status,attempts,notice_count,claimed_at,next_attempt_at) VALUES($1,$2,$3,$4,'uncertain',1,$5,$6,$6)`, org, receipt.ID, receipt.WorkItemID, receipt.UserID, len(notices), now)
			if err != nil {
				return mapError(ctx, err)
			}
		}
		found = true
		result.DeliveryID, result.Attempts = receipt.ID, receipt.Attempts
		recipient, err := s.queries.NotificationRecipient(ctx, pgquery.NotificationRecipientParams{OrganizationID: org, WorkItemID: string(receipt.WorkItemID), UserID: receipt.UserID, ExpiresAt: &now})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && recipient.Email == "" {
			return s.suppressNotificationReceipt(ctx, receipt.ID, now)
		}
		if err != nil {
			return mapError(ctx, err)
		}
		if !retry {
			if err := s.storeNotificationReceiptNotices(ctx, receipt.ID, notices); err != nil {
				return err
			}
		}
		result.Email, result.Title, result.Token = recipient.Email, recipient.Title, recipient.Token
		result.OrganizationSlug, result.ProjectSlug, result.Number = recipient.OrganizationSlug, recipient.ProjectSlug, int(recipient.Number)
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
