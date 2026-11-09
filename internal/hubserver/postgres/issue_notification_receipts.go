package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) notificationReceipt(ctx context.Context, id string) (accountstore.NotificationReceipt, error) {
	var row accountstore.NotificationReceipt
	err := s.db.QueryRow(ctx, `SELECT id,work_item_id,user_id,status,attempts,notice_count,claimed_at,next_attempt_at,finished_at FROM issue_notification_receipts WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(&row.ID, &row.WorkItemID, &row.UserID, &row.Status, &row.Attempts, &row.NoticeCount, &row.ClaimedAt, &row.NextAttemptAt, &row.FinishedAt)
	return row, mapError(ctx, err)
}

func (r *Repository) NotificationReceipt(ctx context.Context, organization tracker.OrganizationID, id string) (accountstore.NotificationReceipt, error) {
	var row accountstore.NotificationReceipt
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		var err error
		row, err = s.notificationReceipt(ctx, id)
		return err
	})
	return row, err
}

func (s *session) rejectedNotification(ctx context.Context, now time.Time) (accountstore.NotificationReceipt, error) {
	var row accountstore.NotificationReceipt
	err := s.db.QueryRow(ctx, `SELECT id,work_item_id,user_id,status,attempts,notice_count,claimed_at,next_attempt_at,finished_at FROM issue_notification_receipts WHERE organization_id=$1 AND status='rejected' AND next_attempt_at<=$2 ORDER BY next_attempt_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.organization, now).Scan(&row.ID, &row.WorkItemID, &row.UserID, &row.Status, &row.Attempts, &row.NoticeCount, &row.ClaimedAt, &row.NextAttemptAt, &row.FinishedAt)
	return row, mapError(ctx, err)
}

func (s *session) notificationReceiptNotices(ctx context.Context, receipt accountstore.NotificationReceipt) ([]pgquery.IssueNotificationNotice, error) {
	rows, err := s.db.Query(ctx, `SELECT source_item,event_id,due_at,state_from,state_to,author,comment_id,related,verify FROM issue_notification_receipt_notices WHERE organization_id=$1 AND receipt_id=$2 ORDER BY due_at,source_item,event_id LIMIT 1001`, s.organization, receipt.ID)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var notices []pgquery.IssueNotificationNotice
	for rows.Next() {
		var notice pgquery.IssueNotificationNotice
		if err := rows.Scan(&notice.SourceItem, &notice.EventID, &notice.DueAt, &notice.StateFrom, &notice.StateTo, &notice.Author, &notice.CommentID, &notice.Related, &notice.Verify); err != nil {
			return nil, mapError(ctx, err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(notices) != receipt.NoticeCount || len(notices) == 0 || len(notices) > 1000 {
		return nil, corestore.ErrRevisionConflict
	}
	return notices, nil
}

func (s *session) storeNotificationReceiptNotices(ctx context.Context, id string, notices []pgquery.IssueNotificationNotice) error {
	sources, events, from, to, authors, comments := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	due := []time.Time{}
	related, verify := []bool{}, []bool{}
	for _, notice := range notices {
		sources, events = append(sources, notice.SourceItem), append(events, notice.EventID)
		due = append(due, notice.DueAt)
		from, to = append(from, notice.StateFrom), append(to, notice.StateTo)
		authors, comments = append(authors, notice.Author), append(comments, notice.CommentID)
		related, verify = append(related, notice.Related), append(verify, notice.Verify)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO issue_notification_receipt_notices(organization_id,receipt_id,source_item,event_id,due_at,state_from,state_to,author,comment_id,related,verify) SELECT $1,$2,n.* FROM unnest($3::text[],$4::text[],$5::timestamptz[],$6::text[],$7::text[],$8::text[],$9::text[],$10::boolean[],$11::boolean[]) AS n(source_item,event_id,due_at,state_from,state_to,author,comment_id,related,verify)`, s.organization, id, sources, events, due, from, to, authors, comments, related, verify)
	return mapError(ctx, err)
}

func (r *Repository) FinishNotification(ctx context.Context, organization tracker.OrganizationID, id string, attempts int64, status string, next, at time.Time) error {
	if id == "" || attempts < 1 || at.IsZero() || next.IsZero() || status != "sent" && status != "rejected" && status != "uncertain" {
		return corestore.ErrInvalidPage
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		tag, err := s.db.Exec(ctx, `UPDATE issue_notification_receipts SET status=$4,next_attempt_at=$5,finished_at=$6 WHERE organization_id=$1 AND id=$2 AND attempts=$3 AND status='uncertain'`, organization, id, attempts, status, next, at)
		if err != nil {
			return mapError(ctx, err)
		}
		if tag.RowsAffected() != 1 {
			current, err := s.notificationReceipt(ctx, id)
			if err != nil {
				return err
			}
			if current.Attempts != attempts || current.Status != status {
				return corestore.ErrRevisionConflict
			}
		}
		if status == "sent" {
			_, err = s.db.Exec(ctx, `DELETE FROM issue_notification_receipt_notices WHERE organization_id=$1 AND receipt_id=$2`, organization, id)
		}
		return mapError(ctx, err)
	})
}

func (s *session) suppressNotificationReceipt(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE issue_notification_receipts SET status='suppressed',finished_at=$3 WHERE organization_id=$1 AND id=$2`, s.organization, id, now)
	if err != nil {
		return mapError(ctx, err)
	}
	_, err = s.db.Exec(ctx, `DELETE FROM issue_notification_receipt_notices WHERE organization_id=$1 AND receipt_id=$2`, s.organization, id)
	return mapError(ctx, err)
}
