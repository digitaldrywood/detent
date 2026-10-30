-- +goose Up
CREATE TABLE storage_conversion_cursors (
    family TEXT PRIMARY KEY,
    turn INTEGER NOT NULL DEFAULT 0,
    positioned INTEGER NOT NULL DEFAULT 0,
    key_1 TEXT NOT NULL DEFAULT '',
    key_2 TEXT NOT NULL DEFAULT '',
    key_3 TEXT NOT NULL DEFAULT '',
    key_4 TEXT NOT NULL DEFAULT '',
    key_5 TEXT NOT NULL DEFAULT '',
    numeric_keys INTEGER NOT NULL DEFAULT 0,
    sequence INTEGER NOT NULL DEFAULT 0,
    examined_rows INTEGER NOT NULL DEFAULT 0,
    examined_bytes INTEGER NOT NULL DEFAULT 0,
    oversized_rows INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX storage_keyset_runs ON collaboration_events(id) WHERE payload_version=0 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision');
CREATE INDEX storage_keyset_sources ON change_sources(version_id) WHERE object_key='';
CREATE INDEX storage_keyset_conversation_records ON conversations(id) WHERE content_attachment_id='';
CREATE INDEX storage_keyset_conversation_questions ON conversation_questions(id) WHERE content_attachment_id='';
CREATE INDEX storage_keyset_conversation_commands ON conversation_commands(conversation_id,key) WHERE content_attachment_id='';
CREATE INDEX storage_keyset_conversation_attachments ON conversation_attachments(id) WHERE content_attachment_id='' AND deleted_at IS NULL;

CREATE INDEX storage_keyset_hot_attempts ON native_attempts(id) WHERE hot_projection_version=0 OR health_disposition_reason IS NULL OR worker_cleanup_required IS NULL;

-- +goose Down
DROP INDEX storage_keyset_hot_attempts;
DROP INDEX storage_keyset_conversation_attachments;
DROP INDEX storage_keyset_conversation_commands;
DROP INDEX storage_keyset_conversation_questions;
DROP INDEX storage_keyset_conversation_records;
DROP INDEX storage_keyset_sources;
DROP INDEX storage_keyset_runs;
DROP TABLE storage_conversion_cursors;
