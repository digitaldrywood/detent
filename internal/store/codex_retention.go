package store

import (
	"context"
	"time"
)

// ProtectedCodexSessions includes all projects because Codex rollout storage is shared.
func (s *sqliteStore) ProtectedCodexSessions(ctx context.Context, cutoff time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider_session_id FROM work_attempts WHERE completed_at IS NULL
 UNION SELECT provider_session_id FROM codex_sessions WHERE completed_at IS NULL OR started_at IS NULL OR julianday(started_at) IS NULL OR julianday(started_at) >= julianday(?) OR julianday(completed_at) >= julianday(?)`, cutoff.UTC().Format(time.RFC3339Nano), cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	protected := map[string]bool{}
	for rows.Next() {
		var id *string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != nil && *id != "" {
			protected[*id] = true
		}
	}
	return protected, rows.Err()
}
