-- +goose Up
CREATE INDEX conversation_messages_compaction_pending_idx
    ON conversation_messages (organization_id,conversation_id,id)
    WHERE delivery IN ('saved','queued','sending','responding');

-- +goose Down
DROP INDEX conversation_messages_compaction_pending_idx;
