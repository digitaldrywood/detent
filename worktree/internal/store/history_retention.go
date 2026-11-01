package store

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

const runtimeHistoryRetention = 30 * 24 * time.Hour
const runtimeHistoryBatch = 256

type RuntimeHistoryMaintainer interface {
	PruneRuntimeHistory(context.Context, time.Time) error
}

const protectedHistoryAttempt = `(status != 'terminal'
 OR (COALESCE(terminal_state, '') = 'abandoned' AND COALESCE(error_class, '') = 'service_restart')
 OR COALESCE(next_action, '') = 'release capacity')`

const unprotectedHistoryIssue = `NOT EXISTS (SELECT 1 FROM work_attempts w
 WHERE w.project_id = t.project_id AND w.issue_id = t.issue_id AND ` + protectedHistoryAttempt + `)`

func (s *sqliteStore) PruneRuntimeHistory(ctx context.Context, now time.Time) error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	cutoff := now.Add(-runtimeHistoryRetention).UTC().Format(time.RFC3339Nano)
	tables := []struct{ candidates, prune string }{
		{`SELECT COALESCE(MAX(id),0) FROM (SELECT id FROM workflow_phase_events WHERE id > ? ORDER BY id LIMIT ?)`,
			`DELETE FROM workflow_phase_events AS t WHERE id > ? AND id <= ? AND julianday(finished_at) < julianday(?) AND ` + unprotectedHistoryIssue + `
   AND NOT EXISTS (SELECT 1 FROM codex_sessions s WHERE s.id = t.session_id AND
    (s.completed_at IS NULL OR s.final_state IN ('running','orphaned')
     OR (s.worker_reaped_at IS NULL AND (s.worker_pid > 0 OR s.worker_cleanup_path IS NOT NULL))))`},
		{`SELECT COALESCE(MAX(id),0) FROM (SELECT id FROM scheduler_decisions WHERE id > ? ORDER BY id LIMIT ?)`,
			`DELETE FROM scheduler_decisions AS t WHERE id > ? AND id <= ? AND julianday(decision_at) < julianday(?) AND ` + unprotectedHistoryIssue},
		{`SELECT COALESCE(MAX(id),0) FROM (SELECT id FROM work_attempts WHERE id > ? ORDER BY id LIMIT ?)`,
			`DELETE FROM work_attempts AS t WHERE id > ? AND id <= ? AND status = 'terminal' AND julianday(completed_at) < julianday(?)
   AND NOT ` + protectedHistoryAttempt + `
   AND NOT EXISTS (SELECT 1 FROM codex_sessions s WHERE s.work_attempt_id = t.id AND
    (s.completed_at IS NULL OR s.final_state IN ('running','orphaned')
     OR (s.worker_reaped_at IS NULL AND (s.worker_pid > 0 OR s.worker_cleanup_path IS NOT NULL))))`},
	}
	for i, table := range tables {
		var end int64
		err := s.db.QueryRowContext(ctx, table.candidates,
			s.historyCursor[i], runtimeHistoryBatch).Scan(&end)
		if err != nil {
			return fmt.Errorf("reading history retention batch: %w", err)
		}
		if end == 0 {
			s.historyCursor[i] = 0
			continue
		}
		_, err = s.db.ExecContext(ctx, table.prune,
			s.historyCursor[i], end, cutoff)
		if err != nil {
			return fmt.Errorf("pruning runtime history: %w", err)
		}
		s.historyCursor[i] = end
		runtime.Gosched()
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum(64)"); err != nil {
		return fmt.Errorf("reclaiming runtime history pages: %w", err)
	}
	return nil
}
