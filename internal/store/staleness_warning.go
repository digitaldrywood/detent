package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *sqliteStore) ListStalenessWarningStates(ctx context.Context, projectID string) ([]StalenessWarningState, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, ErrProjectRequired
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT warning_id, reminded_at, acknowledged_at
FROM staleness_warning_states
WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list staleness warning states: %w", err)
	}
	defer rows.Close()
	states := make([]StalenessWarningState, 0)
	for rows.Next() {
		var warningID string
		var remindedAt sql.NullString
		var acknowledgedAt sql.NullString
		if err := rows.Scan(&warningID, &remindedAt, &acknowledgedAt); err != nil {
			return nil, fmt.Errorf("scan staleness warning state: %w", err)
		}
		state := StalenessWarningState{ProjectID: projectID, WarningID: warningID}
		if remindedAt.Valid {
			parsed, err := parseTimestamp("reminded_at", remindedAt.String)
			if err != nil {
				return nil, err
			}
			state.RemindedAt = &parsed
		}
		if acknowledgedAt.Valid {
			parsed, err := parseTimestamp("acknowledged_at", acknowledgedAt.String)
			if err != nil {
				return nil, err
			}
			state.AcknowledgedAt = &parsed
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate staleness warning states: %w", err)
	}
	return states, nil
}

func (s *sqliteStore) RecordStalenessWarningReminder(ctx context.Context, projectID string, warningID string, at time.Time) error {
	projectID, warningID, err := validatedStalenessWarningIdentity(projectID, warningID)
	if err != nil {
		return err
	}
	timestamp, err := requiredTimestamp("reminded_at", at)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO staleness_warning_states (project_id, warning_id, reminded_at)
VALUES (?, ?, ?)
ON CONFLICT(project_id, warning_id) DO UPDATE SET reminded_at = excluded.reminded_at`, projectID, warningID, timestamp); err != nil {
		return fmt.Errorf("record staleness warning reminder: %w", err)
	}
	return nil
}

func (s *sqliteStore) AcknowledgeStalenessWarning(ctx context.Context, projectID string, warningID string, at time.Time) error {
	projectID, warningID, err := validatedStalenessWarningIdentity(projectID, warningID)
	if err != nil {
		return err
	}
	timestamp, err := requiredTimestamp("acknowledged_at", at)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO staleness_warning_states (project_id, warning_id, acknowledged_at)
VALUES (?, ?, ?)
ON CONFLICT(project_id, warning_id) DO UPDATE SET acknowledged_at = excluded.acknowledged_at`, projectID, warningID, timestamp); err != nil {
		return fmt.Errorf("acknowledge staleness warning: %w", err)
	}
	return nil
}

func validatedStalenessWarningIdentity(projectID string, warningID string) (string, string, error) {
	projectID = strings.TrimSpace(projectID)
	warningID = strings.TrimSpace(warningID)
	if projectID == "" {
		return "", "", ErrProjectRequired
	}
	if warningID == "" {
		return "", "", errors.New("warning_id is required")
	}
	return projectID, warningID, nil
}
