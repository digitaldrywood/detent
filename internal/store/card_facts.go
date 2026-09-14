package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CardHistory contains durable facts that must not depend on runtime history limits.
// Days use UTC, consistently across the dashboard and API.
type CardHistory struct {
	AttemptsToday int64
	LaneReason    string
	LaneReasonAt  *time.Time
}

type CardHistoryStore interface {
	IssueCardHistory(context.Context, IssueIdentity, time.Time) (CardHistory, error)
}

func (s *sqliteStore) IssueCardHistory(ctx context.Context, issue IssueIdentity, now time.Time) (CardHistory, error) {
	day := now.UTC().Truncate(24 * time.Hour)
	var result CardHistory
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM work_attempts
 WHERE project_id = ? AND issue_id = ? AND julianday(started_at) >= julianday(?) AND julianday(started_at) < julianday(?)`,
		issue.ProjectID, issue.IssueID, day.Format(time.RFC3339Nano), day.Add(24*time.Hour).Format(time.RFC3339Nano)).Scan(&result.AttemptsToday)
	if err != nil {
		return CardHistory{}, fmt.Errorf("count card attempts: %w", err)
	}
	var recorded string
	err = s.db.QueryRowContext(ctx, `SELECT reason, recorded_at FROM (
 SELECT reason, written_at AS recorded_at, 1 AS source_priority, id FROM lane_ledger
 WHERE project_id = ? AND issue_id = ? AND result = 'applied'
 UNION ALL
 SELECT COALESCE(reason, ''), started_at, 0, id FROM workflow_phase_events
 WHERE project_id = ? AND issue_id = ? AND phase_type = 'lane' AND status = 'entered'
 ) ORDER BY julianday(recorded_at) DESC, source_priority DESC, id DESC LIMIT 1`,
		issue.ProjectID, issue.IssueID, issue.ProjectID, issue.IssueID).Scan(&result.LaneReason, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return CardHistory{}, fmt.Errorf("read card lane reason: %w", err)
	}
	at, err := parseTimestamp("recorded_at", recorded)
	if err != nil {
		return CardHistory{}, err
	}
	result.LaneReasonAt = &at
	return result, nil
}
