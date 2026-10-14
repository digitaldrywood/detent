package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/schedulehealth"
)

func (s *sqliteStore) LatestScheduledRun(ctx context.Context, projectID string, scheduleID string) (schedulehealth.Run, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT project_id, schedule_id, scheduled_for, started_at, completed_at, COALESCE(error, '')
FROM scheduled_runs
WHERE project_id = ? AND schedule_id = ?
ORDER BY completed_at DESC, id DESC
LIMIT 1`, strings.TrimSpace(projectID), strings.TrimSpace(scheduleID))
	var record schedulehealth.Run
	var scheduledFor string
	var startedAt string
	var completedAt string
	if err := row.Scan(&record.ProjectID, &record.ScheduleID, &scheduledFor, &startedAt, &completedAt, &record.Error); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return schedulehealth.Run{}, false, nil
		}
		return schedulehealth.Run{}, false, fmt.Errorf("read latest scheduled run: %w", err)
	}
	var err error
	record.ScheduledFor, err = parseTimestamp("scheduled_for", scheduledFor)
	if err != nil {
		return schedulehealth.Run{}, false, fmt.Errorf("parse scheduled run scheduled_for: %w", err)
	}
	record.StartedAt, err = parseTimestamp("started_at", startedAt)
	if err != nil {
		return schedulehealth.Run{}, false, fmt.Errorf("parse scheduled run started_at: %w", err)
	}
	record.CompletedAt, err = parseTimestamp("completed_at", completedAt)
	if err != nil {
		return schedulehealth.Run{}, false, fmt.Errorf("parse scheduled run completed_at: %w", err)
	}
	return record, true, nil
}

func (s *sqliteStore) RecordScheduledRun(ctx context.Context, record schedulehealth.Run) error {
	scheduledFor, err := requiredTimestamp("scheduled_for", record.ScheduledFor)
	if err != nil {
		return err
	}
	startedAt, err := requiredTimestamp("started_at", record.StartedAt)
	if err != nil {
		return err
	}
	completedAt, err := requiredTimestamp("completed_at", record.CompletedAt)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO scheduled_runs (project_id, schedule_id, scheduled_for, started_at, completed_at, error)
VALUES (?, ?, ?, ?, ?, ?)`, strings.TrimSpace(record.ProjectID), strings.TrimSpace(record.ScheduleID), scheduledFor, startedAt, completedAt, nullString(record.Error))
	if err != nil {
		return fmt.Errorf("record scheduled run: %w", err)
	}
	return nil
}
