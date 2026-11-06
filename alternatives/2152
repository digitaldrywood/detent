-- +goose Up
ALTER TABLE conversations ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_questions ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_commands ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversation_attachments ADD COLUMN content_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN conversion_state INTEGER NOT NULL DEFAULT 0;
ALTER TABLE conversation_questions ADD COLUMN conversion_state INTEGER NOT NULL DEFAULT 0;
ALTER TABLE conversation_commands ADD COLUMN conversion_state INTEGER NOT NULL DEFAULT 0;
ALTER TABLE conversation_attachments ADD COLUMN conversion_state INTEGER NOT NULL DEFAULT 0;
CREATE INDEX conversations_record_payload_pending ON conversations(conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_questions_record_payload_pending ON conversation_questions(conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_commands_record_payload_pending ON conversation_commands(conversion_state) WHERE conversion_state=0;
CREATE INDEX conversation_attachments_record_payload_pending ON conversation_attachments(conversion_state) WHERE conversion_state=0;
CREATE INDEX conversations_record_payload_queued ON conversations(organization_id,project_id,id) WHERE conversion_state=1;
CREATE INDEX conversation_questions_record_payload_queued ON conversation_questions(conversation_id) WHERE conversion_state=1;
CREATE INDEX conversation_commands_record_payload_queued ON conversation_commands(conversation_id) WHERE conversion_state=1;
CREATE INDEX conversation_attachments_record_payload_queued ON conversation_attachments(conversation_id) WHERE conversion_state=1;
DROP INDEX storage_conversion_schedule;
CREATE INDEX storage_conversion_schedule ON storage_conversion_progress(updated_at,organization_id,project_id,family,aggregate_id) WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files','tool_revisions','conversation_records','conversation_questions','conversation_commands','conversation_attachments');

-- +goose Down
DROP INDEX storage_conversion_schedule;
CREATE INDEX storage_conversion_schedule ON storage_conversion_progress(updated_at,organization_id,project_id,family,aggregate_id) WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files','tool_revisions');
DROP INDEX conversation_attachments_record_payload_queued;
DROP INDEX conversation_commands_record_payload_queued;
DROP INDEX conversation_questions_record_payload_queued;
DROP INDEX conversations_record_payload_queued;
DROP INDEX conversation_attachments_record_payload_pending;
DROP INDEX conversation_commands_record_payload_pending;
DROP INDEX conversation_questions_record_payload_pending;
DROP INDEX conversations_record_payload_pending;
ALTER TABLE conversation_attachments DROP COLUMN content_attachment_id;
ALTER TABLE conversation_commands DROP COLUMN content_attachment_id;
ALTER TABLE conversation_questions DROP COLUMN content_attachment_id;
ALTER TABLE conversations DROP COLUMN content_attachment_id;
ALTER TABLE conversation_attachments DROP COLUMN conversion_state;
ALTER TABLE conversation_commands DROP COLUMN conversion_state;
ALTER TABLE conversation_questions DROP COLUMN conversion_state;
ALTER TABLE conversations DROP COLUMN conversion_state;
