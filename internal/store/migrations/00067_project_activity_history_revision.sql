-- +goose Up
DROP TRIGGER workflow_history_update;

-- +goose StatementBegin
CREATE TRIGGER workflow_history_update AFTER UPDATE ON workflow_phase_events
WHEN OLD.phase_type IS NOT 'agent_activity'
  OR NEW.phase_type IS NOT 'agent_activity'
  OR OLD.id IS NOT NEW.id
  OR OLD.project_id IS NOT NEW.project_id
  OR OLD.issue_id IS NOT NEW.issue_id
  OR OLD.identifier IS NOT NEW.identifier
  OR OLD.issue_url IS NOT NEW.issue_url
  OR OLD.pr_number IS NOT NEW.pr_number
  OR OLD.finished_at IS NOT NEW.finished_at
BEGIN
  UPDATE workflow_history_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER workflow_history_update;

-- +goose StatementBegin
CREATE TRIGGER workflow_history_update AFTER UPDATE ON workflow_phase_events
BEGIN
  UPDATE workflow_history_revision SET revision = revision + 1 WHERE id = 1;
END;
-- +goose StatementEnd
