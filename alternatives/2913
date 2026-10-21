package store

import "context"

// ProtectedCodexThreadIDs includes unfinished sessions and their resume sources.
// Query the whole instance: a profile can serve more than one project.
func (s *sqliteStore) ProtectedCodexThreadIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
 SELECT DISTINCT provider_thread_id FROM codex_sessions
 WHERE provider_thread_id IS NOT NULL AND provider_thread_id <> ''
 AND (completed_at IS NULL OR id IN (
 SELECT resumed_from_session_id FROM codex_sessions WHERE completed_at IS NULL
 ))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
