-- +goose Up
ALTER TABLE conversation_commands ADD COLUMN target_turn_id text NOT NULL DEFAULT '';
CREATE INDEX conversation_commands_coordinator_pending_idx ON conversation_commands (organization_id,conversation_id,key) WHERE kind='cancel' AND receipt_status='saved';

-- +goose Down
DROP INDEX conversation_commands_coordinator_pending_idx;
ALTER TABLE conversation_commands DROP COLUMN target_turn_id;
