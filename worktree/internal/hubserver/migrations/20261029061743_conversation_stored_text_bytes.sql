-- +goose Up
ALTER TABLE conversation_messages ADD COLUMN stored_text_bytes INTEGER CHECK(stored_text_bytes >= 0);
CREATE INDEX conversation_events_delta_suffix ON conversation_events(conversation_id,message_id,delta_offset,seq) WHERE type='message.delta';

-- +goose Down
DROP INDEX conversation_events_delta_suffix;
ALTER TABLE conversation_messages DROP COLUMN stored_text_bytes;
