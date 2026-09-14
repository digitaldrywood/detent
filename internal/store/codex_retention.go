package store

import (
	"context"
	"time"
)

// ProtectedCodexSessions includes all projects because Codex rollout storage is shared.
// Keys include thread IDs (used by rollout metadata) and provider session IDs.
func (s *sqliteStore) ProtectedCodexSessions(ctx context.Context, cutoff time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(a.provider_session_id, ''), s.provider_session_id), s.provider_thread_id
 FROM work_attempts a LEFT JOIN codex_sessions s
 ON s.id = a.detent_session_id OR s.work_attempt_id = a.id
 WHERE a.completed_at IS NULL
 UNION SELECT provider_session_id, provider_thread_id FROM codex_sessions s
 WHERE completed_at IS NULL OR started_at IS NULL OR julianday(started_at) IS NULL
 OR julianday(started_at) >= julianday(?) OR julianday(completed_at) >= julianday(?)`, cutoff.UTC().Format(time.RFC3339Nano), cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	protected := map[string]bool{}
	for rows.Next() {
		var sessionID, threadID *string
		if err := rows.Scan(&sessionID, &threadID); err != nil {
			return nil, err
		}
		if threadID != nil && *threadID != "" {
			protected[*threadID] = true
		}
		if sessionID == nil || *sessionID == "" {
			continue
		}
		protected[*sessionID] = true
		// Codex joins threadID and turnID with a hyphen, and both IDs can contain
		// hyphens. Retain every possible thread prefix when only that composite
		// identity is available (for example, an active attempt without a session
		// row yet). Guessing a single split could delete an active rollout.
		for offset := range *sessionID {
			if offset > 0 && (*sessionID)[offset] == '-' {
				protected[(*sessionID)[:offset]] = true
			}
		}
	}
	return protected, rows.Err()
}
