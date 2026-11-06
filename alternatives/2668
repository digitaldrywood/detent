-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE conversation_attachments ADD COLUMN collaboration_bytes bigint NOT NULL DEFAULT 0;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE conversation_attachments DROP COLUMN collaboration_bytes;
