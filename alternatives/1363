package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
)

func (s *Service) deliverDailySummaries(ctx context.Context, now time.Time) error {
	if s.config.Hosted == nil || s.config.Hosted.EmailSender == nil {
		return nil
	}
	rows, err := s.database.reader.QueryContext(ctx, `WITH due AS MATERIALIZED (SELECT * FROM daily_summary_preferences WHERE organization_id=? AND enabled=1 AND due_at<=? ORDER BY due_at,user_id LIMIT 16)
SELECT p.user_id,p.organization_id,p.send_minute,p.time_zone,p.weekdays,p.window_end,p.token,m.email,m.principal_id,m.role,m.active=1 AND t.revoked_at IS NULL FROM due p JOIN hosted_members m ON m.user_id=p.user_id JOIN api_tokens t ON t.id=m.principal_id ORDER BY p.due_at,p.user_id`, s.config.Hosted.OrganizationID, now.Unix())
	if err != nil {
		return fmt.Errorf("read due summaries: %w", err)
	}
	defer rows.Close()
	recipients := []summaryRecipient{}
	for rows.Next() {
		var r summaryRecipient
		var zone string
		var end int64
		if err := rows.Scan(&r.user, &r.organization, &r.minute, &zone, &r.weekdays, &end, &r.token, &r.email, &r.principal, &r.role, &r.eligible); err != nil {
			return err
		}
		r.zone, err = time.LoadLocation(zone)
		if err != nil {
			return fmt.Errorf("load summary time zone: %w", err)
		}
		r.end = time.Unix(end, 0).UTC()
		recipients = append(recipients, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures error
	for _, r := range recipients {
		if !r.eligible {
			if err := s.advanceDailySummary(ctx, r, now); err != nil {
				failures = errors.Join(failures, err)
			}
			continue
		}
		if err := s.deliverDailySummary(ctx, r, now); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Service) deliverDailySummary(ctx context.Context, r summaryRecipient, now time.Time) error {
	var status string
	err := s.database.reader.QueryRowContext(ctx, `SELECT status FROM daily_summary_receipts WHERE user_id=? AND organization_id=? AND window_end=?`, r.user, r.organization, r.end.Unix()).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && status != "rejected" {
		return s.advanceDailySummary(ctx, r, now)
	}
	data, quiet, err := s.buildDailySummary(ctx, r)
	if err != nil {
		return fmt.Errorf("build daily summary: %w", err)
	}
	var message auth.EmailMessage
	if !quiet {
		message, err = emailtemplate.RenderDailySummary(data)
		if err != nil {
			return err
		}
		message.To = r.email
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	claimStatus := "uncertain"
	if quiet {
		claimStatus = "quiet"
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO daily_summary_receipts(user_id,organization_id,window_end,status,recorded_at)
SELECT user_id,organization_id,window_end,?,? FROM daily_summary_preferences WHERE user_id=? AND organization_id=? AND window_end=? AND enabled=1 AND due_at<=?
ON CONFLICT(user_id,organization_id,window_end) DO UPDATE SET status=excluded.status,attempts=daily_summary_receipts.attempts+1,recorded_at=excluded.recorded_at WHERE daily_summary_receipts.status='rejected'`, claimStatus, now.Unix(), r.user, r.organization, r.end.Unix(), now.Unix())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if quiet {
		return s.advanceDailySummary(ctx, r, now)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sendErr := s.config.Hosted.EmailSender.SendEmail(sendCtx, message)
	cancel()
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	outcome := auth.EmailDeliveryOutcome(sendErr)
	status = "uncertain"
	if outcome == "committed" {
		status = "sent"
	}
	if outcome == "rejected" {
		status = "rejected"
	}
	tx, err = s.database.db.BeginTx(finishCtx, nil)
	if err != nil {
		return errors.Join(sendErr, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(finishCtx, `UPDATE daily_summary_receipts SET status=?,recorded_at=? WHERE user_id=? AND organization_id=? AND window_end=? AND status='uncertain'`, status, now.Unix(), r.user, r.organization, r.end.Unix()); err != nil {
		return errors.Join(sendErr, err)
	}
	if status == "rejected" {
		_, err = tx.ExecContext(finishCtx, `UPDATE daily_summary_preferences SET due_at=?+min(21600,30*(1<<min(10,(SELECT attempts FROM daily_summary_receipts WHERE user_id=? AND organization_id=? AND window_end=?))))+? WHERE user_id=? AND organization_id=? AND window_end=?`, now.Unix(), r.user, r.organization, r.end.Unix(), int64(summaryJitter(r.user, r.organization, now)/time.Second), r.user, r.organization, r.end.Unix())
	} else {
		err = advanceDailySummary(finishCtx, tx, r, now)
	}
	if err != nil {
		return errors.Join(sendErr, err)
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(sendErr, err)
	}
	if sendErr != nil {
		return fmt.Errorf("send daily summary: %w", sendErr)
	}
	return nil
}

func advanceDailySummary(ctx context.Context, q nativeExecer, r summaryRecipient, now time.Time) error {
	next := summarySchedule(maxTime(r.end, now), r.minute, r.zone, r.weekdays, 1)
	_, err := q.ExecContext(ctx, `UPDATE daily_summary_preferences SET window_end=?,due_at=? WHERE user_id=? AND organization_id=? AND window_end=?`, next.Unix(), next.Add(summaryJitter(r.user, r.organization, next)).Unix(), r.user, r.organization, r.end.Unix())
	return err
}

func (s *Service) advanceDailySummary(ctx context.Context, r summaryRecipient, now time.Time) error {
	return advanceDailySummary(ctx, s.database.db, r, now)
}
