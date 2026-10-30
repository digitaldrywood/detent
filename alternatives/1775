-- +goose Up
ALTER TABLE native_commands ADD COLUMN response_object_key TEXT NOT NULL DEFAULT '' CHECK(response_object_key='' OR response_json='{}');

-- +goose Down
CREATE TABLE command_payload_rollback_guard(value INTEGER CHECK(value=0));
INSERT INTO command_payload_rollback_guard SELECT EXISTS(SELECT 1 FROM native_commands WHERE response_object_key<>'');
DROP TABLE command_payload_rollback_guard;
ALTER TABLE native_commands DROP COLUMN response_object_key;
