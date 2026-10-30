-- +goose Up
CREATE TABLE work_attempt_sequence (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  last_id INTEGER NOT NULL
);
INSERT INTO work_attempt_sequence SELECT 1, COALESCE(MAX(id), 0) FROM work_attempts;

-- +goose StatementBegin
CREATE TRIGGER work_attempt_sequence_insert AFTER INSERT ON work_attempts
BEGIN
  UPDATE work_attempt_sequence SET last_id = MAX(last_id, NEW.id) WHERE singleton = 1;
END;
-- +goose StatementEnd

CREATE INDEX work_attempts_retention_protected_idx ON work_attempts(project_id, issue_id)
WHERE status != 'terminal'
 OR (COALESCE(terminal_state, '') = 'abandoned' AND COALESCE(error_class, '') = 'service_restart')
 OR COALESCE(next_action, '') = 'release capacity';

-- +goose Down
DROP INDEX work_attempts_retention_protected_idx;
DROP TRIGGER work_attempt_sequence_insert;
DROP TABLE work_attempt_sequence;
