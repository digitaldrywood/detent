-- +goose Up
ALTER TABLE projects ADD COLUMN aging_enabled INTEGER NOT NULL DEFAULT 0 CHECK (aging_enabled IN (0, 1));
ALTER TABLE issues ADD COLUMN dispatch_wait_since TEXT;

UPDATE issues SET dispatch_wait_since = updated_at
WHERE workflow_state_id IN (SELECT id FROM workflow_states WHERE dispatchable = 1 AND terminal = 0);

-- +goose StatementBegin
CREATE TRIGGER issues_dispatch_wait_insert AFTER INSERT ON issues
BEGIN
  UPDATE issues SET dispatch_wait_since = CASE WHEN EXISTS (
    SELECT 1 FROM workflow_states WHERE id = NEW.workflow_state_id AND dispatchable = 1 AND terminal = 0
  ) THEN NEW.created_at END WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER issues_dispatch_wait_transition AFTER UPDATE OF workflow_state_id ON issues
WHEN OLD.workflow_state_id IS NOT NEW.workflow_state_id
BEGIN
  UPDATE issues SET dispatch_wait_since = CASE WHEN EXISTS (
    SELECT 1 FROM workflow_states WHERE id = NEW.workflow_state_id AND dispatchable = 1 AND terminal = 0
  ) THEN COALESCE(OLD.dispatch_wait_since, NEW.updated_at) END WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER issues_dispatch_wait_transition;
DROP TRIGGER issues_dispatch_wait_insert;
ALTER TABLE issues DROP COLUMN dispatch_wait_since;
ALTER TABLE projects DROP COLUMN aging_enabled;
