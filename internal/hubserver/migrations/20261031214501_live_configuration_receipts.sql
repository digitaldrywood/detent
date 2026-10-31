-- +goose Up
DROP INDEX native_commands_completed;
CREATE INDEX native_commands_completed ON native_commands(organization_id,operation,response_expired,created_at) WHERE response_pending=0 OR response_pending IS NULL;

-- +goose Down
DROP INDEX native_commands_completed;
CREATE INDEX native_commands_completed ON native_commands(organization_id,operation,created_at DESC) WHERE response_pending=0 OR response_pending IS NULL;
