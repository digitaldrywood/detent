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
       proposed_count, filed_count, deduplicated_count, limited_count,
       issues_json, COALESCE(error, '')
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
  proposed_count, filed_count, deduplicated_count, limited_count, issues_json, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(record.ProjectID), strings.TrimSpace(record.RoutineName), scheduledFor, startedAt,
		completedAt, nonNegative(int64(record.Proposed)), nonNegative(int64(record.Filed)),
		nonNegative(int64(record.Deduplicated)), nonNegative(int64(record.Limited)), string(issuesJSON), nullString(record.Error))
	if err != nil {
		return fmt.Errorf("recording routine run: %w", err)
	}
	return nil
}

func (s *sqliteStore) OpenRoutineIssueIDs(ctx context.Context, projectID string, routineName string) (_ []string, resultErr error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT issue_id
FROM routine_findings
WHERE project_id = ? AND routine_name = ? AND open = 1
ORDER BY issue_id`, strings.TrimSpace(projectID), strings.TrimSpace(routineName))
	if err != nil {
		return nil, fmt.Errorf("reading routine issue ids: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	var issueIDs []string
	for rows.Next() {
		var issueID string
		if err := rows.Scan(&issueID); err != nil {
			return nil, err
		}
		issueIDs = append(issueIDs, issueID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return issueIDs, nil
}

func (s *sqliteStore) RecordRoutineIssue(ctx context.Context, projectID string, routineName string, issue routinemodel.IssueRecord) error {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return errors.New("routine issue id is required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO routine_findings (project_id, routine_name, issue_id, identifier, url, open)
VALUES (?, ?, ?, ?, ?, 1)
ON CONFLICT(project_id, routine_name, issue_id) DO UPDATE SET
  identifier = excluded.identifier,
  url = excluded.url,
  open = 1`, strings.TrimSpace(projectID), strings.TrimSpace(routineName), issueID,
		strings.TrimSpace(issue.Identifier), strings.TrimSpace(issue.URL))
	if err != nil {
		return fmt.Errorf("recording routine issue: %w", err)
	}
	return nil
}

func (s *sqliteStore) CloseRoutineIssues(ctx context.Context, projectID string, routineName string, issueIDs []string) error {
	for _, issueID := range issueIDs {
		if strings.TrimSpace(issueID) == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `
UPDATE routine_findings
SET open = 0
WHERE project_id = ? AND routine_name = ? AND issue_id = ?`,
			strings.TrimSpace(projectID), strings.TrimSpace(routineName), strings.TrimSpace(issueID)); err != nil {
			return fmt.Errorf("closing routine issue: %w", err)
		}
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
		&record.Proposed, &record.Filed, &record.Deduplicated, &record.Limited, &issuesJSON, &record.Error,
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
