-- +goose Up
CREATE INDEX conversation_messages_item_idx ON conversation_messages(conversation_id, attempt_id, provider_item_id, seq);

-- +goose Down
DROP INDEX conversation_messages_item_idx;
