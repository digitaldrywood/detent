-- +goose Up
ALTER TABLE native_commands ADD COLUMN response_expired INTEGER NOT NULL DEFAULT 0 CHECK(response_expired IN (0,1));
ALTER TABLE native_commands ADD COLUMN response_replay_until_ns INTEGER;
CREATE INDEX native_commands_body_expiry ON native_commands(response_replay_until_ns) WHERE response_pending=0 AND response_expired=0;
CREATE INDEX native_commands_unknown_horizon ON native_commands(response_replay_until_ns) WHERE response_replay_until_ns IS NULL;
CREATE INDEX native_commands_inline_body ON native_commands(response_replay_until_ns) WHERE response_pending=0 AND response_expired=0 AND response_object_key='' AND response_json<>'{}';
CREATE INDEX native_commands_replay_key ON native_commands(organization_id,actor_id,command_key,operation);

-- +goose Down
CREATE TEMPORARY TABLE command_receipt_rollback_guard (expired INTEGER NOT NULL CHECK(expired=0));
INSERT INTO command_receipt_rollback_guard SELECT EXISTS(SELECT 1 FROM native_commands WHERE response_expired=1);
DROP TABLE command_receipt_rollback_guard;
DROP INDEX native_commands_replay_key;
DROP INDEX native_commands_inline_body;
DROP INDEX native_commands_body_expiry;
DROP INDEX native_commands_unknown_horizon;
ALTER TABLE native_commands DROP COLUMN response_replay_until_ns;
ALTER TABLE native_commands DROP COLUMN response_expired;
