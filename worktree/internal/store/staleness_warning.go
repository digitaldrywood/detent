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
	return listStalenessWarningStates(ctx, s.db, projectID)
}

type stalenessWarningStateQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listStalenessWarningStates(ctx context.Context, querier stalenessWarningStateQuerier, projectID string) ([]StalenessWarningState, error) {
	rows, err := querier.QueryContext(ctx, `
SELECT warning_id, reminded_at, acknowledged_at, last_seen_at
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
		var lastSeenAt sql.NullString
		if err := rows.Scan(&warningID, &remindedAt, &acknowledgedAt, &lastSeenAt); err != nil {
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
		if lastSeenAt.Valid {
			parsed, err := parseTimestamp("last_seen_at", lastSeenAt.String)
			if err != nil {
				return nil, err
			}
			state.LastSeenAt = &parsed
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
INSERT INTO staleness_warning_states (project_id, warning_id, reminded_at, last_seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(project_id, warning_id) DO UPDATE SET
  reminded_at = excluded.reminded_at,
  last_seen_at = excluded.last_seen_at`, projectID, warningID, timestamp, timestamp); err != nil {
		return fmt.Errorf("record staleness warning reminder: %w", err)
	}
	return nil
}

func (s *sqliteStore) AcknowledgeStalenessWarning(ctx context.Context, projectID string, warningID string, at time.Time) error {
	return s.AcknowledgeStalenessWarnings(ctx, projectID, []string{warningID}, at)
}

func (s *sqliteStore) AcknowledgeStalenessWarnings(ctx context.Context, projectID string, warningIDs []string, at time.Time) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ErrProjectRequired
	}
	warningIDs, err := validatedStalenessWarningIDs(warningIDs)
	if err != nil {
		return err
	}
	timestamp, err := requiredTimestamp("acknowledged_at", at)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin staleness warning acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, warningID := range warningIDs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO staleness_warning_states (project_id, warning_id, acknowledged_at, last_seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(project_id, warning_id) DO UPDATE SET
  acknowledged_at = COALESCE(staleness_warning_states.acknowledged_at, excluded.acknowledged_at),
  last_seen_at = excluded.last_seen_at`, projectID, warningID, timestamp, timestamp); err != nil {
			return fmt.Errorf("acknowledge staleness warning %q: %w", warningID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit staleness warning acknowledgement: %w", err)
	}
	return nil
}

func (s *sqliteStore) ReconcileStalenessWarningStates(
	ctx context.Context,
	projectID string,
	activeWarningIDs []string,
	observedAt time.Time,
	inactiveBefore time.Time,
) ([]StalenessWarningState, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, ErrProjectRequired
	}
	activeWarningIDs, err := normalizedStalenessWarningIDs(activeWarningIDs)
	if err != nil {
		return nil, err
	}
	observedTimestamp, err := requiredTimestamp("last_seen_at", observedAt)
	if err != nil {
		return nil, err
	}
	inactiveTimestamp, err := requiredTimestamp("inactive_before", inactiveBefore)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin staleness warning reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
DELETE FROM staleness_warning_states
WHERE project_id = ?
  AND (last_seen_at IS NULL OR last_seen_at < ?)`, projectID, inactiveTimestamp); err != nil {
		return nil, fmt.Errorf("prune inactive staleness warning states: %w", err)
	}
	for _, warningID := range activeWarningIDs {
		if _, err := tx.ExecContext(ctx, `
UPDATE staleness_warning_states
SET last_seen_at = ?
WHERE project_id = ? AND warning_id = ?`, observedTimestamp, projectID, warningID); err != nil {
			return nil, fmt.Errorf("mark staleness warning %q observed: %w", warningID, err)
		}
	}
	states, err := listStalenessWarningStates(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit staleness warning reconciliation: %w", err)
	}
	return states, nil
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

func validatedStalenessWarningIDs(warningIDs []string) ([]string, error) {
	if len(warningIDs) == 0 {
		return nil, errors.New("warning_ids are required")
	}
	return normalizedStalenessWarningIDs(warningIDs)
}

func normalizedStalenessWarningIDs(warningIDs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(warningIDs))
	normalized := make([]string, 0, len(warningIDs))
	for _, warningID := range warningIDs {
		warningID = strings.TrimSpace(warningID)
		if warningID == "" {
			return nil, errors.New("warning_id is required")
		}
		if _, exists := seen[warningID]; exists {
			continue
		}
		seen[warningID] = struct{}{}
		normalized = append(normalized, warningID)
	}
	return normalized, nil
}
