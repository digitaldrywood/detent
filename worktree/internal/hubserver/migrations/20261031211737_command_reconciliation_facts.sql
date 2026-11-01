-- +goose Up
ALTER TABLE native_commands ADD COLUMN response_reconciliation_hash TEXT;
CREATE INDEX native_commands_unknown_reconciliation ON native_commands(response_reconciliation_hash) WHERE operation='urgent_runner_update' AND response_reconciliation_hash IS NULL AND response_expired=0;
DROP INDEX native_commands_inline_body;
CREATE INDEX native_commands_inline_body ON native_commands(response_replay_until_ns) WHERE response_expired=0 AND response_object_key='' AND response_json<>'{}' AND response_json<>'{"detent_pending_command":true}';

-- +goose Down
DROP INDEX native_commands_inline_body;
CREATE INDEX native_commands_inline_body ON native_commands(response_replay_until_ns) WHERE response_pending=0 AND response_expired=0 AND response_object_key='' AND response_json<>'{}';
DROP INDEX native_commands_unknown_reconciliation;
ALTER TABLE native_commands DROP COLUMN response_reconciliation_hash;
