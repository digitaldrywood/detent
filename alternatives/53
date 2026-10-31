-- name: ScratchRetentionState :one
SELECT CAST(COALESCE(w.status = 'terminal', 0) AS INTEGER) AS terminal
FROM codex_sessions s
LEFT JOIN work_attempts w ON w.id = s.work_attempt_id
WHERE s.worker_cleanup_path = ?
ORDER BY s.id DESC
LIMIT 1;
