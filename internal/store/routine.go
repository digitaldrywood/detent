package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	routinemodel "github.com/digitaldrywood/detent/internal/routine/model"
)

func (s *sqliteStore) LatestRoutineRun(ctx context.Context, projectID string, routineName string) (routinemodel.RunRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT project_id, routine_name, scheduled_for, started_at, completed_at,
       filed_count, deduplicated_count, issues_json, COALESCE(error, '')
FROM routine_runs
WHERE project_id = ? AND routine_name = ?
ORDER BY completed_at DESC, id DESC
LIMIT 1`, strings.TrimSpace(projectID), strings.TrimSpace(routineName))
	record, err := scanRoutineRun(row.Scan)
	if errors.Is(err, ErrNotFound) {
		return routinemodel.RunRecord{}, false, nil
	}
	if err != nil {
		return routinemodel.RunRecord{}, false, fmt.Errorf("reading latest routine run: %w", err)
	}
	return record, true, nil
}

func (s *sqliteStore) RecordRoutineRun(ctx context.Context, record routinemodel.RunRecord) error {
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
	issues := record.Issues
	if issues == nil {
		issues = []routinemodel.IssueRecord{}
	}
	issuesJSON, err := json.Marshal(issues)
	if err != nil {
		return fmt.Errorf("encoding routine issues: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO routine_runs (
  project_id, routine_name, scheduled_for, started_at, completed_at,
  filed_count, deduplicated_count, issues_json, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(record.ProjectID), strings.TrimSpace(record.RoutineName), scheduledFor, startedAt,
		completedAt, nonNegative(int64(record.Filed)), nonNegative(int64(record.Deduplicated)), string(issuesJSON),
		nullString(record.Error))
	if err != nil {
		return fmt.Errorf("recording routine run: %w", err)
	}
	return nil
}

type routineRunScan func(...any) error

func scanRoutineRun(scan routineRunScan) (routinemodel.RunRecord, error) {
	var record routinemodel.RunRecord
	var scheduledFor string
	var startedAt string
	var completedAt string
	var issuesJSON string
	if err := scan(
		&record.ProjectID, &record.RoutineName, &scheduledFor, &startedAt, &completedAt,
		&record.Filed, &record.Deduplicated, &issuesJSON, &record.Error,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return routinemodel.RunRecord{}, ErrNotFound
		}
		return routinemodel.RunRecord{}, err
	}
	var err error
	if record.ScheduledFor, err = parseTimestamp("scheduled_for", scheduledFor); err != nil {
		return routinemodel.RunRecord{}, err
	}
	if record.StartedAt, err = parseTimestamp("started_at", startedAt); err != nil {
		return routinemodel.RunRecord{}, err
	}
	if record.CompletedAt, err = parseTimestamp("completed_at", completedAt); err != nil {
		return routinemodel.RunRecord{}, err
	}
	if err := json.Unmarshal([]byte(issuesJSON), &record.Issues); err != nil {
		return routinemodel.RunRecord{}, fmt.Errorf("decoding routine issues: %w", err)
	}
	return record, nil
}
