-- +goose Up
ALTER TABLE project_actions ADD COLUMN deleted_at timestamptz;
ALTER TABLE project_actions DROP CONSTRAINT project_actions_organization_id_project_id_slot_key;
CREATE UNIQUE INDEX project_actions_active_slot ON project_actions(organization_id,project_id,slot) WHERE deleted_at IS NULL;
DROP INDEX project_actions_keybinding_idx;
CREATE UNIQUE INDEX project_actions_keybinding_idx ON project_actions(organization_id,project_id,keybinding) WHERE keybinding<>'' AND deleted_at IS NULL;
CREATE INDEX project_actions_deleted ON project_actions(organization_id,id) WHERE deleted_at IS NOT NULL;

-- +goose Down
DROP INDEX project_actions_deleted;
DROP INDEX project_actions_keybinding_idx;
CREATE UNIQUE INDEX project_actions_keybinding_idx ON project_actions(organization_id,project_id,keybinding) WHERE keybinding<>'';
DROP INDEX project_actions_active_slot;
ALTER TABLE project_actions ADD CONSTRAINT project_actions_organization_id_project_id_slot_key UNIQUE(organization_id,project_id,slot);
ALTER TABLE project_actions DROP COLUMN deleted_at;
