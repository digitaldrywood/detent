package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/retro"
)

func (s *sqliteStore) LoadRetroSnapshot(ctx context.Context, projectID string, since time.Time) (retro.Snapshot, error) {
	projectID = strings.TrimSpace(projectID)
	sinceRaw, err := requiredTimestamp("since", since)
	if err != nil {
		return retro.Snapshot{}, err
	}
	snapshot := retro.Snapshot{}
	if snapshot.Attempts, err = s.loadRetroAttempts(ctx, projectID, sinceRaw); err != nil {
		return retro.Snapshot{}, err
	}
	if snapshot.Sessions, err = s.loadRetroSessions(ctx, projectID, sinceRaw); err != nil {
		return retro.Snapshot{}, err
	}
	if snapshot.UsageEvents, err = s.loadRetroUsageEvents(ctx, projectID, sinceRaw); err != nil {
		return retro.Snapshot{}, err
	}
	if snapshot.PhaseEvents, err = s.loadRetroPhaseEvents(ctx, projectID, sinceRaw); err != nil {
		return retro.Snapshot{}, err
	}
	return snapshot, nil
}

func (s *sqliteStore) RetroFiledOnDay(ctx context.Context, projectID string, day time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT CAST(COALESCE(SUM(filed_count), 0) AS INTEGER)
FROM retro_runs
WHERE project_id = ? AND event_day = ?`, strings.TrimSpace(projectID), day.UTC().Format(time.DateOnly)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("reading retro filed count: %w", err)
	}
	return count, nil
}

func (s *sqliteStore) RecordRetroRun(ctx context.Context, record retro.RunRecord) error {
	startedAt, err := requiredTimestamp("started_at", record.StartedAt)
	if err != nil {
		return err
	}
	completedAt, err := requiredTimestamp("completed_at", record.CompletedAt)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO retro_runs (
  project_id, trigger, started_at, completed_at, findings_count,
  filed_count, updated_count, error, event_day
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(record.ProjectID), strings.TrimSpace(record.Trigger), startedAt, completedAt,
		nonNegative(int64(record.Findings)), nonNegative(int64(record.Filed)), nonNegative(int64(record.Updated)),
		nullString(record.Error), record.CompletedAt.UTC().Format(time.DateOnly))
	if err != nil {
		return fmt.Errorf("recording retro run: %w", err)
	}
	return nil
}

func (s *sqliteStore) loadRetroAttempts(ctx context.Context, projectID string, since string) ([]retro.Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, COALESCE(issue_id, ''), COALESCE(identifier, ''), COALESCE(issue_url, ''),
	       attempt_number, started_at, COALESCE(completed_at, ''), COALESCE(terminal_state, ''),
	       COALESCE(error_class, ''), COALESCE(error_message, ''), COALESCE(phase, ''),
	       COALESCE(status_message, ''), COALESCE(wait_reason, ''),
       COALESCE(capacity_snapshot_json, '{}')
FROM work_attempts
WHERE project_id = ? AND COALESCE(completed_at, started_at) >= ?
ORDER BY started_at, id`, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("reading retro work attempts: %w", err)
	}
	defer rows.Close()
	var attempts []retro.Attempt
	for rows.Next() {
		var attempt retro.Attempt
		var startedAt string
		var completedAt string
		if err := rows.Scan(
			&attempt.ID, &attempt.IssueID, &attempt.Identifier, &attempt.IssueURL,
			&attempt.AttemptNumber, &startedAt, &completedAt, &attempt.TerminalState,
			&attempt.ErrorClass, &attempt.ErrorMessage, &attempt.Phase, &attempt.StatusMessage,
			&attempt.WaitReason, &attempt.CapacitySnapshotJSON,
		); err != nil {
			return nil, err
		}
		attempt.StartedAt, err = parseTimestamp("started_at", startedAt)
		if err != nil {
			return nil, err
		}
		attempt.CompletedAt, err = parseRetroOptionalTimestamp("completed_at", completedAt)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *sqliteStore) loadRetroSessions(ctx context.Context, projectID string, since string) ([]retro.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT s.id, COALESCE(s.work_attempt_id, 0), COALESCE(s.issue_id, ''), COALESCE(s.identifier, ''),
       COALESCE(s.issue_url, ''), COALESCE(s.started_at, ''), COALESCE(s.completed_at, ''),
       COALESCE(s.total_tokens, 0), COALESCE(s.final_state, ''), COALESCE(s.orphan_recovery_outcome, '')
FROM codex_sessions s
WHERE COALESCE(s.completed_at, s.started_at, '') >= ?
  AND (
    EXISTS (SELECT 1 FROM usage_events u WHERE u.session_id = s.id AND u.project_id = ?)
    OR EXISTS (SELECT 1 FROM work_attempts w WHERE w.id = s.work_attempt_id AND w.project_id = ?)
  )
ORDER BY s.started_at, s.id`, since, projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("reading retro sessions: %w", err)
	}
	defer rows.Close()
	var sessions []retro.Session
	for rows.Next() {
		var session retro.Session
		var startedAt string
		var completedAt string
		if err := rows.Scan(
			&session.ID, &session.WorkAttemptID, &session.IssueID, &session.Identifier,
			&session.IssueURL, &startedAt, &completedAt, &session.TotalTokens,
			&session.FinalState, &session.OrphanRecoveryOutcome,
		); err != nil {
			return nil, err
		}
		session.StartedAt, err = parseRetroOptionalTimestamp("started_at", startedAt)
		if err != nil {
			return nil, err
		}
		session.CompletedAt, err = parseRetroOptionalTimestamp("completed_at", completedAt)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *sqliteStore) loadRetroUsageEvents(ctx context.Context, projectID string, since string) ([]retro.UsageEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(issue_id, ''), COALESCE(identifier, ''), finished_at,
       COALESCE(total_tokens, 0), COALESCE(outcome, '')
FROM usage_events
WHERE project_id = ? AND finished_at >= ?
ORDER BY finished_at, id`, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("reading retro usage events: %w", err)
	}
	defer rows.Close()
	var events []retro.UsageEvent
	for rows.Next() {
		var event retro.UsageEvent
		var finishedAt string
		if err := rows.Scan(&event.IssueID, &event.Identifier, &finishedAt, &event.TotalTokens, &event.Outcome); err != nil {
			return nil, err
		}
		event.FinishedAt, err = parseTimestamp("finished_at", finishedAt)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *sqliteStore) loadRetroPhaseEvents(ctx context.Context, projectID string, since string) ([]retro.PhaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(issue_id, ''), COALESCE(identifier, ''), COALESCE(issue_url, ''),
       phase_type, phase_name, COALESCE(reason, ''), COALESCE(status, ''), started_at,
       COALESCE(finished_at, ''), COALESCE(total_tokens, 0)
FROM workflow_phase_events
WHERE project_id = ? AND COALESCE(finished_at, started_at) >= ?
ORDER BY started_at, id`, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("reading retro phase events: %w", err)
	}
	defer rows.Close()
	var events []retro.PhaseEvent
	for rows.Next() {
		var event retro.PhaseEvent
		var startedAt string
		var finishedAt string
		if err := rows.Scan(
			&event.IssueID, &event.Identifier, &event.IssueURL, &event.PhaseType,
			&event.PhaseName, &event.Reason, &event.Status, &startedAt, &finishedAt, &event.TotalTokens,
		); err != nil {
			return nil, err
		}
		event.StartedAt, err = parseTimestamp("started_at", startedAt)
		if err != nil {
			return nil, err
		}
		event.FinishedAt, err = parseRetroOptionalTimestamp("finished_at", finishedAt)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func parseRetroOptionalTimestamp(name string, value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	return parseTimestamp(name, value)
}
