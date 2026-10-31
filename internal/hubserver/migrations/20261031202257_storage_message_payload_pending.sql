-- +goose Up
DROP INDEX conversation_messages_payload_pending;
DROP INDEX conversation_messages_payload_discovery;
CREATE INDEX conversation_messages_payload_pending ON conversation_messages(conversation_id) WHERE content_attachment_id='' OR actor_principal_id IS NULL;
CREATE INDEX conversation_messages_payload_discovery ON conversation_messages(conversion_state) WHERE (content_attachment_id='' OR actor_principal_id IS NULL) AND conversion_state<>1;

-- +goose Down
DROP INDEX conversation_messages_payload_discovery;
DROP INDEX conversation_messages_payload_pending;
CREATE INDEX conversation_messages_payload_pending ON conversation_messages(conversation_id) WHERE actor_principal_id IS NULL;
CREATE INDEX conversation_messages_payload_discovery ON conversation_messages(actor_principal_id) WHERE actor_principal_id IS NULL AND conversion_state<>1;
