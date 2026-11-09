-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE conversation_attachments VALIDATE CONSTRAINT conversation_attachment_pending_publication;

-- +goose Down
SELECT 1;
