-- +goose Up
ALTER TABLE conversation_messages ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';

ALTER TABLE conversation_messages ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;

CREATE INDEX conversation_events_delta_message ON conversation_events(conversation_id,json_extract(body_json,'$.message_id'),seq) WHERE type='message.delta';
CREATE INDEX conversation_starts_conversation_idx ON conversation_starts(conversation_id,attempt_id);

-- +goose Down
DROP INDEX conversation_starts_conversation_idx;
DROP INDEX conversation_events_delta_message;
ALTER TABLE conversation_messages DROP COLUMN revision;
ALTER TABLE conversation_messages DROP COLUMN content_attachment_id;
