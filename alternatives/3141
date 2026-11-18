-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY conversation_messages_pending_user_idx
    ON conversation_messages (organization_id,conversation_id)
    WHERE role='user' AND delivery IN ('saved','queued');

-- +goose Down
DROP INDEX CONCURRENTLY conversation_messages_pending_user_idx;
