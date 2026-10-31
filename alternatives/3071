-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN project_id TEXT;

UPDATE codex_sessions
SET project_id = (
  SELECT work_attempts.project_id
  FROM work_attempts
  WHERE work_attempts.id = codex_sessions.work_attempt_id
)
WHERE work_attempt_id IS NOT NULL
  AND EXISTS (
    SELECT 1
    FROM work_attempts
    WHERE work_attempts.id = codex_sessions.work_attempt_id
      AND trim(work_attempts.project_id) != ''
  );

CREATE INDEX codex_sessions_project_completed_idx
ON codex_sessions(project_id, completed_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS codex_sessions_project_completed_idx;

ALTER TABLE codex_sessions
DROP COLUMN project_id;
